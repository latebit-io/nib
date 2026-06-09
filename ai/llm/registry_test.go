package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeModelsDevPayload() map[string]any {
	return map[string]any{
		"openai": map[string]any{
			"id":   "openai",
			"name": "OpenAI",
			"models": map[string]any{
				"gpt-5.2": map[string]any{
					"id":        "gpt-5.2",
					"name":      "GPT 5.2",
					"family":    "gpt",
					"tool_call": true,
				},
				"gpt-5.4": map[string]any{
					"id":        "gpt-5.4",
					"name":      "GPT 5.4",
					"family":    "gpt",
					"tool_call": true,
				},
				"text-embedding-3-small": map[string]any{
					"id":        "text-embedding-3-small",
					"name":      "Embedding 3 Small",
					"family":    "embedding",
					"tool_call": false,
				},
			},
		},
		"anthropic": map[string]any{
			"id":   "anthropic",
			"name": "Anthropic",
			"models": map[string]any{
				"claude-4-sonnet": map[string]any{
					"id":        "claude-4-sonnet",
					"name":      "Claude 4 Sonnet",
					"family":    "claude",
					"tool_call": true,
				},
			},
		},
	}
}

func startFakeModelsDev(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(fakeModelsDevPayload()); err != nil {
			t.Errorf("encode fake payload: %v", err)
		}
	}))
}

func newTestRegistry(t *testing.T, srvURL string, ttl time.Duration) *ModelRegistry {
	t.Helper()
	return &ModelRegistry{
		cacheDir: t.TempDir(),
		ttl:      ttl,
		url:      srvURL,
	}
}

func TestModelRegistry_FetchAndFilter(t *testing.T) {
	srv := startFakeModelsDev(t)
	defer srv.Close()

	r := newTestRegistry(t, srv.URL, time.Hour)
	ctx := context.Background()

	tests := []struct {
		name      string
		provider  string
		wantCount int
		wantIDs   []string
		wantErr   bool
	}{
		{
			name:      "openai filters to tool_call models",
			provider:  "openai",
			wantCount: 2,
			wantIDs:   []string{"gpt-5.2", "gpt-5.4"},
		},
		{
			name:      "anthropic returns claude",
			provider:  "anthropic",
			wantCount: 1,
			wantIDs:   []string{"claude-4-sonnet"},
		},
		{
			name:     "unknown provider returns error",
			provider: "unknown",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			models, err := r.Models(ctx, tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(models) != tt.wantCount {
				t.Errorf("got %d models, want %d", len(models), tt.wantCount)
			}
			ids := make(map[string]bool)
			for _, m := range models {
				ids[m.ID] = true
			}
			for _, wantID := range tt.wantIDs {
				if !ids[wantID] {
					t.Errorf("missing model %q", wantID)
				}
			}
		})
	}
}

func TestModelRegistry_CustomFilter(t *testing.T) {
	srv := startFakeModelsDev(t)
	defer srv.Close()

	r := newTestRegistry(t, srv.URL, time.Hour)

	filter := func(m RegistryModel) bool {
		return m.ID == "gpt-5.4"
	}
	models, err := r.Models(context.Background(), "openai", filter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.4" {
		t.Errorf("got %v, want [gpt-5.4]", models)
	}
}

func TestModelRegistry_DiskCachePersistence(t *testing.T) {
	srv := startFakeModelsDev(t)
	defer srv.Close()

	dir := t.TempDir()

	r1 := &ModelRegistry{cacheDir: dir, ttl: time.Hour, url: srv.URL}
	if _, err := r1.Models(context.Background(), "openai"); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, registryCacheFile)); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	// New registry with unreachable URL loads from disk.
	r2 := &ModelRegistry{cacheDir: dir, ttl: time.Hour, url: "http://192.0.2.1:1/unreachable"}
	models, err := r2.Models(context.Background(), "openai")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("got %d models, want 2", len(models))
	}
}

func TestModelRegistry_StaleCache_FallsBack(t *testing.T) {
	// Unreachable server — forces fetch failure.
	r := &ModelRegistry{
		cacheDir: t.TempDir(),
		ttl:      time.Millisecond,
		url:      "http://192.0.2.1:1/unreachable",
	}
	r.mu.Lock()
	r.cache = &registryCache{
		FetchedAt: time.Now().Add(-time.Hour),
		Providers: map[string][]RegistryModel{
			"test": {{ID: "old", Name: "Old", ToolCall: true}},
		},
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	models, err := r.Models(ctx, "test")
	if err != nil {
		t.Fatalf("expected fallback to stale cache, got error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "old" {
		t.Errorf("got %v, want [old]", models)
	}
}

func TestModelRegistry_Refresh(t *testing.T) {
	srv := startFakeModelsDev(t)
	defer srv.Close()

	r := newTestRegistry(t, srv.URL, 0)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	models, err := r.Models(context.Background(), "openai")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("got %d models, want 2", len(models))
	}
}

func TestModelRegistry_SnapshotFallback(t *testing.T) {
	// No disk cache, unreachable server — must fall back to embedded snapshot.
	r := &ModelRegistry{
		cacheDir: t.TempDir(),
		ttl:      time.Hour,
		url:      "http://192.0.2.1:1/unreachable",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	models, err := r.Models(ctx, "openai")
	if err != nil {
		t.Fatalf("expected snapshot fallback, got error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("snapshot returned zero models for openai")
	}
	found := false
	for _, m := range models {
		if m.ID == "gpt-5.2" {
			found = true
			break
		}
	}
	if !found {
		t.Error("snapshot missing gpt-5.2")
	}
}

func TestLoadSnapshot(t *testing.T) {
	snap := loadSnapshot()
	if snap == nil {
		t.Fatal("loadSnapshot returned nil")
	}
	if len(snap.Providers) == 0 {
		t.Fatal("snapshot has no providers")
	}
	for _, pid := range []string{"openai", "anthropic", "google", "minimax", "openrouter", "github-copilot"} {
		models := snap.Providers[pid]
		if len(models) == 0 {
			t.Errorf("snapshot missing provider %q", pid)
		}
	}
}
