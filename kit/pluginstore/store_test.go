package pluginstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeFetcher copies a fixture tree and returns a settable pin so store
// install/update logic is exercised without the network.
type fakeFetcher struct {
	srcDir string
	pin    string
}

func (f *fakeFetcher) Fetch(_ context.Context, _ Source, dest string) (string, error) {
	if err := copyTree(f.srcDir, dest); err != nil {
		return "", err
	}
	return f.pin, nil
}

// writePlugin creates a fixture plugin dir with a manifest at root.
func writePlugin(t *testing.T, dir, name, version string) {
	t.Helper()
	manifestDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","version":"` + version + `"}`
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A representative component file so copy/promote moves real content.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")

	// Missing file → fresh registry.
	r, err := loadRegistry(path)
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	if r.Version != registryVersion || len(r.Plugins) != 0 {
		t.Fatalf("fresh registry = %+v", r)
	}

	r.upsertPlugin(InstalledPlugin{ID: "foo", Name: "foo", Source: LocalSource("/x"), Enabled: true, Trusted: true})
	if err := r.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadRegistry(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].ID != "foo" || !got.Plugins[0].Trusted {
		t.Fatalf("round-trip mismatch: %+v", got.Plugins)
	}
}

func TestStore_InstallListRemove(t *testing.T) {
	t.Parallel()
	fixture := filepath.Join(t.TempDir(), "src")
	writePlugin(t, fixture, "foo", "1.0.0")

	root := t.TempDir()
	st, err := New(root, WithFetcher(&fakeFetcher{srcDir: fixture, pin: "sha1"}))
	if err != nil {
		t.Fatal(err)
	}

	ent, err := st.Install(context.Background(), LocalSource(fixture), InstallOptions{Enabled: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ent.ID != "foo" || ent.Version != "1.0.0" || !ent.Enabled || ent.Pin != "sha1" {
		t.Fatalf("entry = %+v", ent)
	}
	// Source tree promoted with content.
	if _, err := os.Stat(filepath.Join(st.SourceDir("foo"), "README.md")); err != nil {
		t.Fatalf("promoted source missing README: %v", err)
	}
	// Registry persisted.
	if _, err := os.Stat(st.registryPath()); err != nil {
		t.Fatalf("registry.json not written: %v", err)
	}
	if list := st.List(); len(list) != 1 || list[0].ID != "foo" {
		t.Fatalf("List = %+v", list)
	}

	if err := st.Remove("foo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if list := st.List(); len(list) != 0 {
		t.Fatalf("List after remove = %+v", list)
	}
	if _, err := os.Stat(st.SourceDir("foo")); !os.IsNotExist(err) {
		t.Fatalf("source dir should be gone, stat err=%v", err)
	}
}

func TestStore_UpdatePinChange(t *testing.T) {
	t.Parallel()
	fixture := filepath.Join(t.TempDir(), "src")
	writePlugin(t, fixture, "foo", "1.0.0")

	ff := &fakeFetcher{srcDir: fixture, pin: "sha1"}
	st, err := New(t.TempDir(), WithFetcher(ff))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Install(context.Background(), LocalSource(fixture), InstallOptions{Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Same pin → no-op.
	changed, err := st.Update(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Update(same): %v", err)
	}
	if changed {
		t.Errorf("expected no change when pin unchanged")
	}

	// New pin → swap + persist.
	ff.pin = "sha2"
	changed, err = st.Update(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Update(new): %v", err)
	}
	if !changed {
		t.Errorf("expected change when pin moved")
	}
	if got, _ := st.Get("foo"); got.Pin != "sha2" {
		t.Errorf("pin = %q, want sha2", got.Pin)
	}

	if _, err := st.Update(context.Background(), "missing"); err == nil {
		t.Errorf("expected error updating unknown plugin")
	}
}

func TestStore_TrustSurvivesReinstall(t *testing.T) {
	t.Parallel()
	fixture := filepath.Join(t.TempDir(), "src")
	writePlugin(t, fixture, "foo", "1.0.0")

	st, err := New(t.TempDir(), WithFetcher(&fakeFetcher{srcDir: fixture, pin: "sha1"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Install(context.Background(), LocalSource(fixture), InstallOptions{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTrusted("foo", true); err != nil {
		t.Fatal(err)
	}

	// Reinstall (e.g. a manual re-import) must preserve the trust grant.
	ent, err := st.Install(context.Background(), LocalSource(fixture), InstallOptions{Enabled: true})
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !ent.Trusted {
		t.Errorf("trust grant lost on reinstall: %+v", ent)
	}
}

// TestStore_MarketplaceFlow exercises the full loop through a real local
// copy fetch: add a marketplace, install a catalog plugin by name.
func TestStore_MarketplaceFlow(t *testing.T) {
	t.Parallel()
	mktRoot := filepath.Join(t.TempDir(), "mp")
	// Marketplace manifest listing one local plugin.
	mktManifest := filepath.Join(mktRoot, ".claude-plugin")
	if err := os.MkdirAll(mktManifest, 0o755); err != nil {
		t.Fatal(err)
	}
	cat := `{"name":"mp","plugins":[{"name":"foo","source":"./plugins/foo"}]}`
	if err := os.WriteFile(filepath.Join(mktManifest, "marketplace.json"), []byte(cat), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, filepath.Join(mktRoot, "plugins", "foo"), "foo", "2.0.0")

	st, err := New(t.TempDir()) // DefaultFetcher → real local copy
	if err != nil {
		t.Fatal(err)
	}
	mkt, err := st.AddMarketplace(context.Background(), LocalSource(mktRoot))
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if mkt.Name != "mp" || len(mkt.Plugins) != 1 {
		t.Fatalf("marketplace = %+v", mkt)
	}
	if ms := st.ListMarketplaces(); len(ms) != 1 || ms[0].Name != "mp" {
		t.Fatalf("ListMarketplaces = %+v", ms)
	}

	ent, err := st.InstallFromMarketplace(context.Background(), "mp", "foo")
	if err != nil {
		t.Fatalf("InstallFromMarketplace: %v", err)
	}
	if ent.ID != "mp__foo" || ent.Marketplace != "mp" || ent.Version != "2.0.0" {
		t.Fatalf("entry = %+v", ent)
	}
	if _, err := os.Stat(filepath.Join(st.SourceDir("mp__foo"), "README.md")); err != nil {
		t.Fatalf("installed source missing content: %v", err)
	}
}
