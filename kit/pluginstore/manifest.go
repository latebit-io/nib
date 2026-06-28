package pluginstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// manifestRelPath is the location of a CC plugin manifest within a
// plugin directory. Only this file lives under .claude-plugin/; the
// components (skills, commands, …) sit at the plugin root.
const manifestRelPath = ".claude-plugin/plugin.json"

// marketplaceRelPath is the location of a CC marketplace manifest
// within a marketplace repository.
const marketplaceRelPath = ".claude-plugin/marketplace.json"

// Manifest is the subset of a Claude Code plugin.json that nib needs to
// import and track a plugin. Unknown fields are ignored, so forward-
// compatible additions upstream do not break parsing; the converter
// (M1+) reads the richer component frontmatter directly.
type Manifest struct {
	// Name is the plugin's kebab-case identifier (required by CC).
	Name string `json:"name"`
	// Version is an optional semantic version. Absent when the plugin is
	// pinned by commit SHA instead.
	Version string `json:"version,omitempty"`
	// DisplayName is the human-readable name; falls back to Name.
	DisplayName string `json:"displayName,omitempty"`
	// Description is the one-line summary shown in plugin listings.
	Description string `json:"description,omitempty"`
}

// ParseManifest decodes a plugin.json body. A missing name is an error
// because every CC plugin requires one and the store keys on it.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("pluginstore: parse plugin manifest: %w", err)
	}
	if m.Name == "" {
		return Manifest{}, fmt.Errorf("pluginstore: plugin manifest missing required \"name\"")
	}
	return m, nil
}

// ReadManifest reads and parses <pluginDir>/.claude-plugin/plugin.json.
// A plugin directory without a manifest is reported via [os.ErrNotExist]
// (wrapped) so callers can distinguish "no manifest" from a parse error
// and fall back to default-directory auto-discovery if they choose.
func ReadManifest(pluginDir string) (Manifest, error) {
	path := filepath.Join(pluginDir, manifestRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("pluginstore: no manifest at %s: %w", path, os.ErrNotExist)
		}
		return Manifest{}, fmt.Errorf("pluginstore: read manifest %s: %w", path, err)
	}
	return ParseManifest(data)
}

// Marketplace is the subset of a Claude Code marketplace.json nib needs
// to list and resolve member plugins.
type Marketplace struct {
	// Name identifies the marketplace.
	Name string `json:"name"`
	// Description is an optional summary.
	Description string `json:"description,omitempty"`
	// Plugins is the catalog of member plugins.
	Plugins []MarketplaceEntry `json:"plugins"`
}

// MarketplaceEntry is one plugin listed in a marketplace catalog.
type MarketplaceEntry struct {
	// Name is the plugin identifier within the marketplace.
	Name string `json:"name"`
	// Description is an optional summary.
	Description string `json:"description,omitempty"`
	// Version is an optional pin shown in listings.
	Version string `json:"version,omitempty"`
	// Source locates the plugin. CC encodes this as either a bare
	// relative-path string or a typed object; both decode into the
	// normalized [Source] via [MarketplaceEntry.UnmarshalJSON].
	Source Source `json:"source"`
}

// ccSourceObject mirrors the object form of a CC marketplace source.
// CC nests the discriminator under the "source" key (e.g.
// {"source":"github","repo":...}); this maps those shapes onto nib's
// flat [Source].
type ccSourceObject struct {
	Source   string `json:"source"`
	Repo     string `json:"repo"`
	URL      string `json:"url"`
	Path     string `json:"path"`
	Ref      string `json:"ref"`
	Package  string `json:"package"`
	Version  string `json:"version"`
	Registry string `json:"registry"`
}

// entryAlias avoids infinite recursion when custom-unmarshaling
// MarketplaceEntry: the Source field is decoded separately from a raw
// message, the rest via the struct's default decoding.
type entryAlias struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Source      json.RawMessage `json:"source"`
}

// UnmarshalJSON normalizes CC's polymorphic "source" (string or object)
// into a [Source]. A bare string is a marketplace-relative path; an
// object is mapped by its inner "source" discriminator.
func (e *MarketplaceEntry) UnmarshalJSON(data []byte) error {
	var a entryAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("pluginstore: parse marketplace entry: %w", err)
	}
	e.Name = a.Name
	e.Description = a.Description
	e.Version = a.Version
	if a.Name == "" {
		return fmt.Errorf("pluginstore: marketplace entry missing name")
	}
	if len(a.Source) == 0 {
		return fmt.Errorf("pluginstore: marketplace entry %q missing source", a.Name)
	}

	// String form → relative local path within the marketplace repo.
	var asString string
	if err := json.Unmarshal(a.Source, &asString); err == nil {
		e.Source = LocalSource(asString)
	} else {
		var obj ccSourceObject
		if err := json.Unmarshal(a.Source, &obj); err != nil {
			return fmt.Errorf("pluginstore: parse source for entry %q: %w", a.Name, err)
		}
		src, err := sourceFromCC(obj)
		if err != nil {
			return fmt.Errorf("pluginstore: entry %q: %w", a.Name, err)
		}
		e.Source = src
	}
	// Reject a structurally-broken source at parse time rather than
	// letting bad catalog data persist and fail only at install.
	if err := e.Source.Validate(); err != nil {
		return fmt.Errorf("pluginstore: entry %q: %w", a.Name, err)
	}
	return nil
}

// sourceFromCC maps a CC source object onto a normalized [Source].
func sourceFromCC(obj ccSourceObject) (Source, error) {
	switch obj.Source {
	case "github":
		return Source{Type: SourceGitHub, Repo: obj.Repo, Ref: obj.Ref}, nil
	case "url", "git":
		return Source{Type: SourceGit, URL: obj.URL, Ref: obj.Ref}, nil
	case "git-subdir":
		return Source{Type: SourceGit, URL: obj.URL, Subdir: obj.Path, Ref: obj.Ref}, nil
	case "npm":
		return Source{Type: SourceNPM, Package: obj.Package, Ref: obj.Version, Registry: obj.Registry}, nil
	case "local", "":
		// Bare object with only a path behaves like the string form.
		if obj.Path == "" {
			return Source{}, fmt.Errorf("local source missing path")
		}
		return LocalSource(obj.Path), nil
	default:
		return Source{}, fmt.Errorf("unknown source kind %q", obj.Source)
	}
}

// ParseMarketplace decodes a marketplace.json body.
func ParseMarketplace(data []byte) (Marketplace, error) {
	var m Marketplace
	if err := json.Unmarshal(data, &m); err != nil {
		return Marketplace{}, fmt.Errorf("pluginstore: parse marketplace: %w", err)
	}
	if m.Name == "" {
		return Marketplace{}, fmt.Errorf("pluginstore: marketplace missing required \"name\"")
	}
	return m, nil
}

// ReadMarketplace reads <repoDir>/.claude-plugin/marketplace.json.
func ReadMarketplace(repoDir string) (Marketplace, error) {
	path := filepath.Join(repoDir, marketplaceRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return Marketplace{}, fmt.Errorf("pluginstore: read marketplace %s: %w", path, err)
	}
	return ParseMarketplace(data)
}
