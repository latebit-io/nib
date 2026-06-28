package pluginstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithinDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{base, outside, filepath.Join(base, "child", "grand")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"child dir", filepath.Join(base, "child"), true},
		{"grandchild", filepath.Join(base, "child", "grand"), true},
		{"base itself", base, true},
		{"not-yet-created leaf under base", filepath.Join(base, "newleaf"), true},
		{"lexical traversal", filepath.Join(base, "..", "outside"), false},
		{"sibling", outside, false},
	}
	for _, tc := range cases {
		if got := withinDir(base, tc.target); got != tc.want {
			t.Errorf("%s: withinDir(base, %q) = %t, want %t", tc.name, tc.target, got, tc.want)
		}
	}

	// Security case the lexical check missed: a symlink that sits under
	// base textually but dereferences outside it must be rejected.
	escape := filepath.Join(base, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if withinDir(base, escape) {
		t.Errorf("withinDir must reject a symlink escaping the base dir")
	}
	if withinDir(base, filepath.Join(escape, "x")) {
		t.Errorf("withinDir must reject a path under an escaping symlink")
	}
}

func TestSwapDir_ReplacesAndPreservesOnSuccess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	final := filepath.Join(root, "final")
	if err := os.MkdirAll(final, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(final, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(root, "staged")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := swapDir(staged, final); err != nil {
		t.Fatalf("swapDir: %v", err)
	}
	// New content present, old gone, no leftover backup.
	if _, err := os.Stat(filepath.Join(final, "new.txt")); err != nil {
		t.Errorf("new content missing after swap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(final, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old content should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(final + ".old"); !os.IsNotExist(err) {
		t.Errorf("backup should be cleaned up, stat err=%v", err)
	}
}

func TestLoadRegistry_RejectsNewerVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(`{"version":999,"plugins":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRegistry(path); err == nil {
		t.Errorf("expected error loading a newer-than-supported registry version")
	}
}
