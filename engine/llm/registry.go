package llm

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

//go:embed models_snapshot.json
var modelsSnapshot []byte

const (
	modelsDevURL      = "https://models.dev/api.json"
	registryCacheFile = "models_cache.json"
)

// RegistryModel describes a model from the models.dev registry.
type RegistryModel struct {
	// ID is the model slug used in API requests.
	ID string `json:"id"`
	// Name is the human-readable display name.
	Name string `json:"name"`
	// Family groups related models (e.g. "gpt-codex", "gpt-pro").
	Family string `json:"family"`
	// ToolCall indicates whether the model supports function calling.
	ToolCall bool `json:"tool_call"`
}

// registryCache is the on-disk cache format.
type registryCache struct {
	// FetchedAt is when the cache was last refreshed from models.dev.
	FetchedAt time.Time `json:"fetched_at"`
	// Providers maps provider ID to its model list.
	Providers map[string][]RegistryModel `json:"providers"`
}

// ModelRegistry fetches and caches model metadata from the models.dev registry.
// It provides a dynamic model list that updates without code changes.
type ModelRegistry struct {
	cacheDir string
	ttl      time.Duration
	url      string

	mu    sync.RWMutex
	cache *registryCache
}

// NewModelRegistry creates a registry that caches to the given directory.
// The TTL controls how often the cache is refreshed from the network.
// A zero TTL means the cache never expires (manual refresh only).
func NewModelRegistry(cacheDir string, ttl time.Duration) *ModelRegistry {
	return &ModelRegistry{
		cacheDir: cacheDir,
		ttl:      ttl,
		url:      modelsDevURL,
	}
}

// ModelFilter is an optional predicate applied after the tool_call check.
// Return true to include the model in results.
type ModelFilter func(m RegistryModel) bool

// Models returns the cached models for a provider, refreshing if the cache
// is stale or missing. Only models with tool_call support are returned.
// An optional filter further narrows results (e.g. codex-family only).
func (r *ModelRegistry) Models(ctx context.Context, providerID string, filters ...ModelFilter) ([]ModelInfo, error) {
	var filter ModelFilter
	if len(filters) > 0 {
		filter = filters[0]
	}

	r.mu.RLock()
	cache := r.cache
	r.mu.RUnlock()

	if cache == nil {
		if loaded, err := r.loadDiskCache(); err == nil && loaded != nil {
			r.mu.Lock()
			r.cache = loaded
			cache = loaded
			r.mu.Unlock()
		}
	}

	if cache == nil || r.isStale(cache) {
		fetched, err := r.fetch(ctx)
		if err != nil {
			if cache != nil {
				return r.filterModels(cache, providerID, filter), nil
			}
			// Network and disk both failed — use the embedded snapshot.
			if snap := loadSnapshot(); snap != nil {
				return r.filterModels(snap, providerID, filter), nil
			}
			return nil, fmt.Errorf("model registry: %w", err)
		}
		r.mu.Lock()
		r.cache = fetched
		cache = fetched
		if err := r.saveDiskCache(fetched); err != nil {
			slog.Warn("model registry: cache write failed", "err", err)
		}
		r.mu.Unlock()
	}

	models := r.filterModels(cache, providerID, filter)
	if len(models) == 0 {
		return nil, fmt.Errorf("model registry: no models for provider %q", providerID)
	}
	return models, nil
}

// Refresh forces a network fetch regardless of TTL. Returns an error if the
// fetch fails; the existing cache is preserved on failure.
func (r *ModelRegistry) Refresh(ctx context.Context) error {
	fetched, err := r.fetch(ctx)
	if err != nil {
		return fmt.Errorf("model registry refresh: %w", err)
	}
	r.mu.Lock()
	r.cache = fetched
	r.mu.Unlock()
	return r.saveDiskCache(fetched)
}

func (r *ModelRegistry) isStale(c *registryCache) bool {
	if r.ttl == 0 {
		return false
	}
	return time.Since(c.FetchedAt) > r.ttl
}

func (r *ModelRegistry) filterModels(c *registryCache, providerID string, filter ModelFilter) []ModelInfo {
	raw := c.Providers[providerID]
	var out []ModelInfo
	for _, m := range raw {
		if !m.ToolCall {
			continue
		}
		if filter != nil && !filter(m) {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, ModelInfo{ID: m.ID, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// modelsDevResponse is the top-level structure of https://models.dev/api.json.
// Each key is a provider ID mapping to provider metadata.
type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Family   string `json:"family"`
	ToolCall bool   `json:"tool_call"`
}

func (r *ModelRegistry) fetch(ctx context.Context) (*registryCache, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev: HTTP %d", resp.StatusCode)
	}

	const maxResponseBytes = 10 << 20 // 10 MB
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	providers := make(map[string][]RegistryModel)
	for providerID, data := range raw {
		var p modelsDevProvider
		if err := json.Unmarshal(data, &p); err != nil {
			slog.Debug("model registry: skip provider", "id", providerID, "err", err)
			continue
		}
		if len(p.Models) == 0 {
			continue
		}
		models := make([]RegistryModel, 0, len(p.Models))
		for _, m := range p.Models {
			models = append(models, RegistryModel(m))
		}
		providers[providerID] = models
	}

	return &registryCache{
		FetchedAt: time.Now(),
		Providers: providers,
	}, nil
}

func (r *ModelRegistry) cachePath() string {
	return filepath.Join(r.cacheDir, registryCacheFile)
}

func (r *ModelRegistry) loadDiskCache() (*registryCache, error) {
	data, err := os.ReadFile(r.cachePath())
	if err != nil {
		return nil, err
	}
	var c registryCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *ModelRegistry) saveDiskCache(c *registryCache) error {
	if err := os.MkdirAll(r.cacheDir, 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	tmp := r.cachePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(tmp, r.cachePath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}

// loadSnapshot parses the embedded models_snapshot.json as a registryCache.
// Returns nil if the snapshot cannot be parsed.
func loadSnapshot() *registryCache {
	var providers map[string][]RegistryModel
	if err := json.Unmarshal(modelsSnapshot, &providers); err != nil {
		return nil
	}
	return &registryCache{
		Providers: providers,
	}
}
