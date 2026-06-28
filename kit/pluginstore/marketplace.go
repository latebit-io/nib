package pluginstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// marketplacesSubdir holds fetched marketplace repositories, kept apart
// from plugin source trees under the store root.
const marketplacesSubdir = ".marketplaces"

func (s *Store) marketplaceDir(name string) string {
	return filepath.Join(s.root, marketplacesSubdir, deriveID("", name))
}

// AddMarketplace fetches a marketplace repository from src, parses its
// catalog, and records it in the registry so member plugins can be
// installed by name. Re-adding a marketplace refreshes its fetched copy.
func (s *Store) AddMarketplace(ctx context.Context, src Source) (Marketplace, error) {
	if err := src.Validate(); err != nil {
		return Marketplace{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	staged, pin, err := s.fetchMarketplaceStaged(ctx, src)
	if err != nil {
		return Marketplace{}, err
	}
	defer func() { _ = os.RemoveAll(staged) }() // best-effort staging cleanup

	mkt, err := ReadMarketplace(staged)
	if err != nil {
		return Marketplace{}, err
	}

	final := s.marketplaceDir(mkt.Name)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return Marketplace{}, fmt.Errorf("pluginstore: create marketplaces dir: %w", err)
	}
	if err := os.RemoveAll(final); err != nil {
		return Marketplace{}, fmt.Errorf("pluginstore: clear %s: %w", final, err)
	}
	if err := os.Rename(staged, final); err != nil {
		return Marketplace{}, fmt.Errorf("pluginstore: promote marketplace %s: %w", mkt.Name, err)
	}

	ref := MarketplaceRef{Name: mkt.Name, Source: src, Pin: pin}
	if i := s.reg.marketplaceIndex(mkt.Name); i >= 0 {
		s.reg.Marketplaces[i] = ref
	} else {
		s.reg.Marketplaces = append(s.reg.Marketplaces, ref)
	}
	if err := s.reg.save(s.registryPath()); err != nil {
		return Marketplace{}, err
	}
	return mkt, nil
}

// ListMarketplaces returns the known marketplaces, sorted by name.
func (s *Store) ListMarketplaces() []MarketplaceRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.reg.Marketplaces)
	slices.SortFunc(out, func(a, b MarketplaceRef) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// InstallFromMarketplace installs the plugin named pluginName from the
// previously-added marketplace named marketplaceName. Relative (local)
// plugin sources in the catalog are resolved against the marketplace's
// fetched directory; remote sources (git/github) are installed directly.
func (s *Store) InstallFromMarketplace(ctx context.Context, marketplaceName, pluginName string) (InstalledPlugin, error) {
	s.mu.Lock()
	if s.reg.marketplaceIndex(marketplaceName) < 0 {
		s.mu.Unlock()
		return InstalledPlugin{}, fmt.Errorf("pluginstore: marketplace %q not added", marketplaceName)
	}
	mktDir := s.marketplaceDir(marketplaceName)
	s.mu.Unlock()

	mkt, err := ReadMarketplace(mktDir)
	if err != nil {
		return InstalledPlugin{}, err
	}
	i := slices.IndexFunc(mkt.Plugins, func(e MarketplaceEntry) bool { return e.Name == pluginName })
	if i < 0 {
		return InstalledPlugin{}, fmt.Errorf("pluginstore: marketplace %q has no plugin %q", marketplaceName, pluginName)
	}
	entry := mkt.Plugins[i]

	src := entry.Source
	if src.Type == SourceLocal {
		// Catalog-relative path → absolute under the fetched marketplace.
		src.Path = filepath.Join(mktDir, filepath.Clean(src.Path))
	}
	return s.Install(ctx, src, InstallOptions{
		Name:        entry.Name,
		Marketplace: marketplaceName,
		Enabled:     true,
	})
}

// fetchMarketplaceStaged fetches a marketplace repo into a staging dir,
// returning the staging path (caller removes it) and resolved pin.
func (s *Store) fetchMarketplaceStaged(ctx context.Context, src Source) (staged, pin string, err error) {
	base := filepath.Join(s.root, marketplacesSubdir)
	if err = os.MkdirAll(base, 0o755); err != nil {
		return "", "", fmt.Errorf("pluginstore: create marketplaces dir: %w", err)
	}
	staged, err = os.MkdirTemp(base, ".staging-*")
	if err != nil {
		return "", "", fmt.Errorf("pluginstore: create staging dir: %w", err)
	}
	pin, err = s.fetcher.Fetch(ctx, src, staged)
	if err != nil {
		_ = os.RemoveAll(staged)
		return "", "", err
	}
	if src.Subdir != "" {
		staged = filepath.Join(staged, filepath.Clean(src.Subdir))
	}
	return staged, pin, nil
}
