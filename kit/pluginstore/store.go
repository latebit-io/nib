package pluginstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Store is the managed plugin store rooted at a directory. It owns the
// registry ledger and the on-disk layout, and serializes mutations so
// concurrent installs/updates cannot corrupt the ledger.
type Store struct {
	root    string
	fetcher Fetcher
	mu      sync.Mutex
	reg     *Registry
}

// Option configures a [Store].
type Option func(*Store)

// WithFetcher overrides the default fetch strategy. Tests inject a
// network-free fetcher; production uses [DefaultFetcher].
func WithFetcher(f Fetcher) Option {
	return func(s *Store) { s.fetcher = f }
}

// New opens (or initializes) a store at root, loading registry.json if
// present. The directory is created lazily on first write.
func New(root string, opts ...Option) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("pluginstore: empty store root")
	}
	s := &Store{root: root, fetcher: DefaultFetcher{}}
	for _, opt := range opts {
		opt(s)
	}
	reg, err := loadRegistry(s.registryPath())
	if err != nil {
		return nil, err
	}
	s.reg = reg
	return s, nil
}

func (s *Store) registryPath() string { return filepath.Join(s.root, "registry.json") }
func (s *Store) srcDir(id string) string {
	return filepath.Join(s.root, "store", id)
}

// ConvertedDir is the nib-native output directory for a plugin, written
// by the converter on install; exported so loader integration can
// locate it.
func (s *Store) ConvertedDir(id string) string { return filepath.Join(s.root, "converted", id) }

// DataDir is the plugin's persistent data directory (the nib equivalent
// of ${CLAUDE_PLUGIN_DATA}). Preserved across updates and removed only
// on uninstall.
func (s *Store) DataDir(id string) string { return filepath.Join(s.root, "data", id) }

// SourceDir is the raw fetched upstream plugin directory.
func (s *Store) SourceDir(id string) string { return s.srcDir(id) }

// InstallOptions tune an install.
type InstallOptions struct {
	// Name overrides the plugin name. When empty the name is read from
	// the fetched manifest. Required when the plugin ships no manifest.
	Name string
	// Marketplace records the owning marketplace (empty for a direct
	// install).
	Marketplace string
	// Enabled sets the initial enabled state. Plugins install enabled by
	// default; pass a pointer-free false via DisabledByDefault if needed.
	Enabled bool
}

// Install fetches a plugin from src and records it in the registry,
// returning the resulting ledger entry. Reinstalling an existing plugin
// (same derived ID) replaces its source tree but preserves the user's
// enabled/trusted/userConfig state and its data directory.
//
// Conversion runs against the staging tree before the source and
// converted trees are promoted, so a conversion failure leaves any
// prior install fully intact rather than half-replaced.
func (s *Store) Install(ctx context.Context, src Source, opts InstallOptions) (InstalledPlugin, error) {
	if err := src.Validate(); err != nil {
		return InstalledPlugin{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.fetchStaged(ctx, src, opts.Name)
	if err != nil {
		return InstalledPlugin{}, err
	}
	defer func() { _ = os.RemoveAll(st.cleanupRoot) }() // always clear the whole staging tree

	id, err := deriveID(opts.Marketplace, st.name)
	if err != nil {
		return InstalledPlugin{}, err
	}

	entry := InstalledPlugin{
		ID:          id,
		Name:        st.name,
		Marketplace: opts.Marketplace,
		Source:      src,
		Pin:         st.pin,
		Version:     st.version,
		Enabled:     opts.Enabled,
	}
	// Carry forward survive-on-update state from a prior install. Enabled
	// is preserved too: a reinstall must not silently re-enable a plugin
	// the user disabled. (Direct index lookup — we already hold s.mu.)
	if i := s.reg.pluginIndex(id); i >= 0 {
		prev := s.reg.Plugins[i]
		entry.Enabled = prev.Enabled
		entry.Trusted = prev.Trusted
		entry.UserConfig = prev.UserConfig
	}

	if err := s.promoteInstall(id, st.name, st.version, st.dir); err != nil {
		return InstalledPlugin{}, err
	}
	s.reg.upsertPlugin(entry)
	if err := s.reg.save(s.registryPath()); err != nil {
		return InstalledPlugin{}, err
	}
	return entry, nil
}

// Update re-fetches an installed plugin from its recorded source and
// swaps in the new tree if the resolved pin changed. It reports whether
// the source tree actually changed. User state and the data directory
// are preserved. Local sources have no pin and are always re-copied
// (reported as changed).
//
// As in [Install], conversion runs before promotion, so a failed update
// leaves the existing install untouched rather than partially upgraded.
func (s *Store) Update(ctx context.Context, id string) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.reg.pluginIndex(id)
	if i < 0 {
		return false, fmt.Errorf("pluginstore: plugin %q not installed", id)
	}
	prev := s.reg.Plugins[i]

	st, err := s.fetchStaged(ctx, prev.Source, prev.Name)
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(st.cleanupRoot) }() // always clear the whole staging tree

	// Git sources with an unchanged SHA are a no-op; local sources lack a
	// pin so we always promote and report changed.
	if st.pin != "" && st.pin == prev.Pin {
		return false, nil
	}
	if err := s.promoteInstall(id, prev.Name, st.version, st.dir); err != nil {
		return false, err
	}
	prev.Pin = st.pin
	prev.Version = st.version
	s.reg.Plugins[i] = prev
	if err := s.reg.save(s.registryPath()); err != nil {
		return false, err
	}
	return true, nil
}

// promoteInstall converts the staged source tree into a temporary
// converted tree, then atomically swaps both the source and converted
// trees into place. Converting first means a failure here never replaces
// or removes the live install.
func (s *Store) promoteInstall(id, name, version, stagedDir string) error {
	// Convert from the staged tree into a temporary converted tree so a
	// conversion failure touches nothing live.
	convParent := filepath.Join(s.root, "converted")
	if err := os.MkdirAll(convParent, 0o755); err != nil {
		return fmt.Errorf("pluginstore: create converted dir: %w", err)
	}
	tmpConv, err := os.MkdirTemp(convParent, ".staging-*")
	if err != nil {
		return fmt.Errorf("pluginstore: create converted staging dir: %w", err)
	}
	cleanupConv := true
	defer func() {
		if cleanupConv {
			_ = os.RemoveAll(tmpConv) // best-effort rollback; the primary error is already being returned
		}
	}()
	if err := s.convertInto(id, name, version, stagedDir, tmpConv); err != nil {
		return err
	}

	// Both conversions succeeded: promote source then converted. swapDir
	// preserves the prior tree until each rename lands.
	if err := swapDir(stagedDir, s.srcDir(id)); err != nil {
		return err
	}
	if err := swapDir(tmpConv, s.ConvertedDir(id)); err != nil {
		return err
	}
	cleanupConv = false
	return nil
}

// Remove uninstalls a plugin: deletes its source, converted, and data
// directories and drops its registry entry. Removing an unknown plugin
// is a no-op.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.reg.pluginIndex(id)
	if i < 0 {
		return nil
	}
	for _, dir := range []string{s.srcDir(id), s.ConvertedDir(id), s.DataDir(id)} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("pluginstore: remove %s: %w", dir, err)
		}
	}
	s.reg.Plugins = slices.Delete(s.reg.Plugins, i, i+1)
	return s.reg.save(s.registryPath())
}

// SetEnabled toggles a plugin's enabled state and persists it.
func (s *Store) SetEnabled(id string, enabled bool) error {
	return s.mutatePlugin(id, func(p *InstalledPlugin) { p.Enabled = enabled })
}

// SetTrusted records (or revokes) the per-plugin trust grant and
// persists it. Consumed by the shell/hook gate from M2 onward.
func (s *Store) SetTrusted(id string, trusted bool) error {
	return s.mutatePlugin(id, func(p *InstalledPlugin) { p.Trusted = trusted })
}

// List returns a copy of the installed plugins, sorted by ID for stable
// output.
func (s *Store) List() []InstalledPlugin {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.reg.Plugins)
	slices.SortFunc(out, func(a, b InstalledPlugin) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Get returns the ledger entry for id and whether it exists.
func (s *Store) Get(id string) (InstalledPlugin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.reg.pluginIndex(id); i >= 0 {
		return s.reg.Plugins[i], true
	}
	return InstalledPlugin{}, false
}

// ActivePlugin describes an enabled plugin's converted component
// locations, for the wiring layer to feed into the skill, command, and
// MCP loaders.
type ActivePlugin struct {
	// ID is the store key.
	ID string
	// Name is the plugin's manifest name.
	Name string
	// SkillsDir is the converted skills root (<converted>/skills).
	SkillsDir string
	// CommandsDir is the converted commands dir (<converted>/commands).
	CommandsDir string
	// AgentsDir is the converted subagent-definitions dir
	// (<converted>/agents).
	AgentsDir string
	// MCPConfigPath is the converted MCP config (<converted>/.mcp.json).
	MCPConfigPath string
	// HooksConfigPath is the converted hooks config
	// (<converted>/hooks/hooks.json).
	HooksConfigPath string
}

// ActivePlugins returns the converted component locations of every
// enabled plugin, sorted by ID. Disabled plugins are omitted. The paths
// may not all exist (a plugin without commands has no commands dir); the
// loaders treat a missing path as empty.
func (s *Store) ActivePlugins() []ActivePlugin {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ActivePlugin
	for _, p := range s.reg.Plugins {
		if !p.Enabled {
			continue
		}
		conv := s.ConvertedDir(p.ID)
		out = append(out, ActivePlugin{
			ID:              p.ID,
			Name:            p.Name,
			SkillsDir:       filepath.Join(conv, "skills"),
			CommandsDir:     filepath.Join(conv, "commands"),
			AgentsDir:       filepath.Join(conv, "agents"),
			MCPConfigPath:   filepath.Join(conv, ".mcp.json"),
			HooksConfigPath: filepath.Join(conv, "hooks", "hooks.json"),
		})
	}
	slices.SortFunc(out, func(a, b ActivePlugin) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// mutatePlugin applies fn to the plugin with id under lock and persists.
func (s *Store) mutatePlugin(id string, fn func(*InstalledPlugin)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.reg.pluginIndex(id)
	if i < 0 {
		return fmt.Errorf("pluginstore: plugin %q not installed", id)
	}
	fn(&s.reg.Plugins[i])
	return s.reg.save(s.registryPath())
}

// importReportName is the converted-tree file holding the last
// conversion report for a plugin.
const importReportName = "import-report.json"

// convertInto builds the nib-native converted tree for a plugin from the
// given source directory into dstDir, freezing import-time variables to
// the plugin's FINAL installed location (not the staging path, so frozen
// ${…_PLUGIN_ROOT}/${…_PLUGIN_DATA} paths are correct at runtime), and
// writes the conversion report into dstDir. dstDir is a caller-owned
// temporary tree swapped into place only after this succeeds.
func (s *Store) convertInto(id, name, version, srcDir, dstDir string) error {
	srcAbs, err := filepath.Abs(s.srcDir(id))
	if err != nil {
		return fmt.Errorf("pluginstore: resolve source path: %w", err)
	}
	dataAbs, err := filepath.Abs(s.DataDir(id))
	if err != nil {
		return fmt.Errorf("pluginstore: resolve data path: %w", err)
	}
	vars := Vars{PluginRoot: srcAbs, PluginData: dataAbs}
	report, err := Convert(srcDir, dstDir, Manifest{Name: name, Version: version}, vars)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("pluginstore: marshal import report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, importReportName), data, 0o644); err != nil {
		return fmt.Errorf("pluginstore: write import report: %w", err)
	}
	return nil
}

// ImportReport returns the conversion report recorded for an installed
// plugin — what converted and what nib cannot yet honor. It reads the
// persisted report so callers (e.g. `/plugin` output) need no live
// conversion.
func (s *Store) ImportReport(id string) (ConvertReport, error) {
	data, err := os.ReadFile(filepath.Join(s.ConvertedDir(id), importReportName))
	if err != nil {
		return ConvertReport{}, fmt.Errorf("pluginstore: read import report for %q: %w", id, err)
	}
	var report ConvertReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ConvertReport{}, fmt.Errorf("pluginstore: parse import report for %q: %w", id, err)
	}
	return report, nil
}

// staged carries the result of a fetch into a temporary tree.
type staged struct {
	// cleanupRoot is the .staging-* directory the caller must remove. It
	// is distinct from dir so a subdir-scoped source still cleans up the
	// whole fetched repo, not just the promoted subdir.
	cleanupRoot string
	// dir is the effective plugin directory (the subdir when the source
	// scopes one, else cleanupRoot). This is what gets promoted.
	dir     string
	pin     string
	name    string
	version string
}

// fetchStaged fetches src into a fresh staging directory and resolves
// the plugin's name and version. nameOverride, when non-empty, wins over
// the manifest (and lets manifest-less plugins install). On any error
// the staging tree is removed; on success the caller owns cleanupRoot.
func (s *Store) fetchStaged(ctx context.Context, src Source, nameOverride string) (staged, error) {
	storeDir := filepath.Join(s.root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return staged{}, fmt.Errorf("pluginstore: create store dir: %w", err)
	}
	root, err := os.MkdirTemp(storeDir, ".staging-*")
	if err != nil {
		return staged{}, fmt.Errorf("pluginstore: create staging dir: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root) // remove the whole staging tree on failure
		}
	}()

	pin, err := s.fetcher.Fetch(ctx, src, root)
	if err != nil {
		return staged{}, err
	}

	// Scope to a git subdir when requested, guarding against traversal
	// that would point outside the fetched repo.
	dir := root
	if src.Subdir != "" {
		dir = filepath.Join(root, filepath.Clean(src.Subdir))
		if !withinDir(root, dir) {
			return staged{}, fmt.Errorf("pluginstore: subdir %q escapes the fetched repo", src.Subdir)
		}
	}

	name, version, err := resolveIdentity(dir, nameOverride)
	if err != nil {
		return staged{}, err
	}

	ok = true
	return staged{cleanupRoot: root, dir: dir, pin: pin, name: name, version: version}, nil
}

// resolveIdentity determines the plugin name and version from the fetched
// tree. A manifest is read when present; nameOverride supplies the name
// for manifest-less plugins (and is required there in M0).
func resolveIdentity(dir, nameOverride string) (name, version string, err error) {
	m, mErr := ReadManifest(dir)
	switch {
	case mErr == nil:
		name = m.Name
		version = m.Version
	case errors.Is(mErr, os.ErrNotExist):
		// Manifest-less plugin: acceptable only with an explicit name.
		if nameOverride == "" {
			return "", "", fmt.Errorf("pluginstore: plugin at %s has no manifest and no name was supplied", dir)
		}
	default:
		return "", "", mErr
	}
	if nameOverride != "" {
		name = nameOverride
	}
	if name == "" {
		return "", "", fmt.Errorf("pluginstore: could not determine plugin name at %s", dir)
	}
	return name, version, nil
}

// idSanitizer strips characters unsafe for a filesystem directory name.
var idSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// idSeparator joins marketplace and plugin name in a store id. It is
// reserved: no component may contain it, and Trim strips leading/trailing
// "_" from components, so "<mkt>__<name>" has exactly one occurrence and
// the id decomposes unambiguously (no direct plugin can spell a
// marketplace-qualified id, and no two pairs can meet on one id).
const idSeparator = "__"

// deriveID builds the stable store key. Marketplace-sourced plugins are
// namespaced "<marketplace>__<name>" so two marketplaces can ship a
// plugin of the same name without colliding on disk.
//
// Each component is sanitized and validated on its own: a name (or
// marketplace) made only of separator characters would otherwise vanish
// from the joined key and collide with another plugin's directory
// ("mp" + "---" must not become "mp"), and neither may contain the
// reserved [idSeparator] (a direct plugin "mp__foo" must not alias the
// marketplace plugin "mp"/"foo").
func deriveID(marketplace, name string) (string, error) {
	nameID, err := sanitizeIDComponent("name", name)
	if err != nil {
		return "", err
	}
	if marketplace == "" {
		return nameID, nil
	}
	mktID, err := sanitizeIDComponent("marketplace", marketplace)
	if err != nil {
		return "", err
	}
	return mktID + idSeparator + nameID, nil
}

// sanitizeIDComponent maps one id component to filesystem-safe form and
// rejects it when nothing survives or it contains the reserved separator.
func sanitizeIDComponent(kind, raw string) (string, error) {
	id := strings.Trim(idSanitizer.ReplaceAllString(raw, "-"), "-._")
	if id == "" {
		return "", fmt.Errorf("pluginstore: %s %q yields an empty store id", kind, raw)
	}
	if strings.Contains(id, idSeparator) {
		return "", fmt.Errorf("pluginstore: %s %q contains reserved separator %q", kind, raw, idSeparator)
	}
	return id, nil
}
