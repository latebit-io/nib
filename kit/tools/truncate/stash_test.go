package truncate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectStash_LazyDirCreation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stash, err := NewProjectStash(root)
	if err != nil {
		t.Fatalf("new stash: %v", err)
	}
	// No Stash call yet — directory must not exist.
	if _, err := os.Stat(stash.Dir()); !os.IsNotExist(err) {
		t.Fatalf("expected stash dir not to exist before first write, stat err = %v", err)
	}
	// First stash creates it.
	rel, err := stash.Stash("bash", "hello")
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	abs := filepath.Join(root, rel)
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read stash: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("stash body = %q, want hello", body)
	}
	if !strings.HasPrefix(rel, ".project/tooltmp/") {
		t.Fatalf("stash path %q not under .project/tooltmp/", rel)
	}
	if !strings.HasSuffix(rel, "-bash.txt") {
		t.Fatalf("stash path %q missing -bash.txt suffix", rel)
	}
}

func TestProjectStash_UniquePerCall(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stash, err := NewProjectStash(root)
	if err != nil {
		t.Fatalf("new stash: %v", err)
	}
	a, err := stash.Stash("bash", "one")
	if err != nil {
		t.Fatalf("stash 1: %v", err)
	}
	b, err := stash.Stash("bash", "two")
	if err != nil {
		t.Fatalf("stash 2: %v", err)
	}
	if a == b {
		t.Fatalf("expected unique paths, both = %q", a)
	}
}

func TestProjectStash_Cleanup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stash, err := NewProjectStash(root)
	if err != nil {
		t.Fatalf("new stash: %v", err)
	}
	if _, err := stash.Stash("bash", "x"); err != nil {
		t.Fatalf("stash: %v", err)
	}
	if _, err := os.Stat(stash.Dir()); err != nil {
		t.Fatalf("expected dir to exist post-stash: %v", err)
	}
	stash.Cleanup()
	if _, err := os.Stat(stash.Dir()); !os.IsNotExist(err) {
		t.Fatalf("expected dir removed after Cleanup, stat err = %v", err)
	}
	// Idempotent.
	stash.Cleanup()
}

func TestProjectStash_NilSafe(t *testing.T) {
	t.Parallel()
	var s *ProjectStash
	if _, err := s.Stash("x", "y"); err == nil {
		t.Fatal("expected error from nil stash")
	}
	// Cleanup on nil must not panic.
	s.Cleanup()
}

func TestProjectStash_PruneStale(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stashRoot := filepath.Join(root, stashSubdir)
	// Build a stale sibling run-id directory.
	stale := filepath.Join(stashRoot, "deadbeef")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// Build a fresh sibling run-id directory — must survive.
	fresh := filepath.Join(stashRoot, "feedface")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatalf("mkdir fresh: %v", err)
	}

	stash, err := NewProjectStash(root)
	if err != nil {
		t.Fatalf("new stash: %v", err)
	}
	_ = stash

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("expected stale dir removed, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("expected fresh dir kept: %v", err)
	}
}

func TestProjectStash_SanitizeLabel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stash, err := NewProjectStash(root)
	if err != nil {
		t.Fatalf("new stash: %v", err)
	}
	// Label contains path traversal + odd chars; must be neutralized
	// to a filesystem-safe suffix.
	rel, err := stash.Stash("../etc/passwd", "x")
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if strings.Contains(rel, "..") {
		t.Fatalf("sanitized path still contains ..: %q", rel)
	}
	if strings.Contains(rel, "/etc/") {
		t.Fatalf("sanitized path leaked traversal: %q", rel)
	}
}
