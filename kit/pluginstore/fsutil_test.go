package pluginstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithinDir(t *testing.T) {
	t.Parallel()
	base := "/a/b"
	cases := []struct {
		target string
		want   bool
	}{
		{"/a/b/c", true},
		{"/a/b", true},
		{"/a/b/c/d", true},
		{"/a/b/../c", false},
		{"/a", false},
		{"/a/bb", false},
	}
	for _, tc := range cases {
		if got := withinDir(base, tc.target); got != tc.want {
			t.Errorf("withinDir(%q,%q) = %t, want %t", base, tc.target, got, tc.want)
		}
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
