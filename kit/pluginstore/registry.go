package pluginstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// registryVersion is the schema version of registry.json. Bumped only
// on a breaking change to the on-disk shape so a future loader can
// migrate older files.
const registryVersion = 1

// InstalledPlugin is one tracked plugin in the store ledger. It records
// where the plugin came from and the exact commit it resolved to so a
// re-sync can detect upstream changes, plus the user-controlled state
// (enabled, trusted, config) that must survive updates.
type InstalledPlugin struct {
	// ID is the stable store key and on-disk directory name. Derived
	// from the plugin name (and marketplace, when installed from one).
	ID string `json:"id"`
	// Name is the plugin's manifest name.
	Name string `json:"name"`
	// Marketplace is the owning marketplace name, empty for a direct
	// install.
	Marketplace string `json:"marketplace,omitempty"`
	// Source locates the upstream plugin for re-sync.
	Source Source `json:"source"`
	// Pin is the resolved commit SHA (git sources) at last fetch.
	Pin string `json:"pin,omitempty"`
	// Version is the manifest version at last fetch, if any.
	Version string `json:"version,omitempty"`
	// Enabled reports whether the plugin's artifacts are active. Survives
	// re-sync.
	Enabled bool `json:"enabled"`
	// Trusted records that the user approved this plugin to run shell /
	// hooks (the per-plugin trust gate). Survives re-sync; re-prompted
	// only when the source identity changes. Honored from M2 onward.
	Trusted bool `json:"trusted"`
	// UserConfig holds resolved values for the plugin's userConfig
	// fields. Survives re-sync. Consumed from M5 onward.
	UserConfig map[string]string `json:"userConfig,omitempty"`
}

// Registry is the persisted store ledger: which marketplaces are known
// and which plugins are installed. It is nib's first durable settings
// file. Not safe for concurrent mutation; the owning [Store] serializes
// access.
type Registry struct {
	// Version is the on-disk schema version.
	Version int `json:"version"`
	// Marketplaces are the known marketplace sources, keyed by name.
	Marketplaces []MarketplaceRef `json:"marketplaces"`
	// Plugins are the installed plugins.
	Plugins []InstalledPlugin `json:"plugins"`
}

// MarketplaceRef records an added marketplace and how to re-fetch it.
type MarketplaceRef struct {
	// Name identifies the marketplace.
	Name string `json:"name"`
	// Source locates the marketplace repository/directory.
	Source Source `json:"source"`
	// Pin is the resolved commit SHA (git sources) at last fetch.
	Pin string `json:"pin,omitempty"`
}

// loadRegistry reads registry.json from path. A missing file yields a
// fresh empty registry — first run is not an error.
func loadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Registry{Version: registryVersion}, nil
		}
		return nil, fmt.Errorf("pluginstore: read registry %s: %w", path, err)
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("pluginstore: parse registry %s: %w", path, err)
	}
	if r.Version == 0 {
		r.Version = registryVersion
	}
	// Refuse a registry written by a newer nib: loading then save()-ing it
	// would silently rewrite it as the older schema and could drop fields
	// the newer version added.
	if r.Version > registryVersion {
		return nil, fmt.Errorf("pluginstore: registry %s is version %d, newer than supported %d; upgrade nib", path, r.Version, registryVersion)
	}
	return &r, nil
}

// save writes the registry to path atomically (temp file + rename) so a
// crash mid-write cannot corrupt the ledger.
func (r *Registry) save(path string) error {
	r.Version = registryVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("pluginstore: create store dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("pluginstore: marshal registry: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("pluginstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup of the orphaned temp file
		return fmt.Errorf("pluginstore: rename %s → %s: %w", tmp, path, err)
	}
	return nil
}

// pluginIndex returns the index of the plugin with id, or -1.
func (r *Registry) pluginIndex(id string) int {
	return slices.IndexFunc(r.Plugins, func(p InstalledPlugin) bool { return p.ID == id })
}

// marketplaceIndex returns the index of the marketplace named n, or -1.
func (r *Registry) marketplaceIndex(n string) int {
	return slices.IndexFunc(r.Marketplaces, func(m MarketplaceRef) bool { return m.Name == n })
}

// upsertPlugin inserts or replaces p by ID, preserving nothing — callers
// carry forward survive-on-update fields before calling.
func (r *Registry) upsertPlugin(p InstalledPlugin) {
	if i := r.pluginIndex(p.ID); i >= 0 {
		r.Plugins[i] = p
		return
	}
	r.Plugins = append(r.Plugins, p)
}
