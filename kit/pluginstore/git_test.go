package pluginstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGitT runs git in dir, failing the test on error.
func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestGitFetch_InstallAndResync proves the managed loop against a real
// local git repo: install pins the current HEAD, a no-op update detects
// no change, and a new upstream commit is picked up on re-sync.
func TestGitFetch_InstallAndResync(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	runGitT(t, repo, "init")
	runGitT(t, repo, "config", "user.email", "test@example.com")
	runGitT(t, repo, "config", "user.name", "Test")
	writePlugin(t, repo, "foo", "1.0.0")
	runGitT(t, repo, "add", "-A")
	runGitT(t, repo, "commit", "-m", "initial")
	head1 := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ent, err := st.Install(context.Background(), GitSource(repo, ""), InstallOptions{Enabled: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ent.Pin != head1 {
		t.Fatalf("pin = %q, want HEAD %q", ent.Pin, head1)
	}

	// No upstream change → re-sync is a no-op.
	changed, err := st.Update(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Update(no change): %v", err)
	}
	if changed {
		t.Errorf("expected no change before a new commit")
	}

	// New upstream commit → re-sync picks it up and re-pins.
	if err := os.WriteFile(filepath.Join(repo, "NEW.md"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repo, "add", "-A")
	runGitT(t, repo, "commit", "-m", "second")
	head2 := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))

	changed, err = st.Update(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Update(new commit): %v", err)
	}
	if !changed {
		t.Errorf("expected change after new commit")
	}
	if got, _ := st.Get("foo"); got.Pin != head2 {
		t.Errorf("re-synced pin = %q, want %q", got.Pin, head2)
	}
	// The new file is present in the swapped-in source tree.
	if _, err := os.Stat(filepath.Join(st.SourceDir("foo"), "NEW.md")); err != nil {
		t.Errorf("re-synced tree missing new file: %v", err)
	}
}

// TestGitFetch_PinnedRef covers the ref path (full clone + detached
// checkout with --end-of-options) and proves a leading-dash ref is
// rejected before git ever runs.
func TestGitFetch_PinnedRef(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	runGitT(t, repo, "init")
	runGitT(t, repo, "config", "user.email", "test@example.com")
	runGitT(t, repo, "config", "user.name", "Test")
	writePlugin(t, repo, "foo", "1.0.0")
	runGitT(t, repo, "add", "-A")
	runGitT(t, repo, "commit", "-m", "initial")
	head1 := strings.TrimSpace(runGitT(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "NEW.md"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repo, "add", "-A")
	runGitT(t, repo, "commit", "-m", "second")

	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ent, err := st.Install(context.Background(), GitSource(repo, head1), InstallOptions{Enabled: true})
	if err != nil {
		t.Fatalf("Install pinned: %v", err)
	}
	if ent.Pin != head1 {
		t.Fatalf("pin = %q, want %q", ent.Pin, head1)
	}
	if _, err := os.Stat(filepath.Join(st.SourceDir("foo"), "NEW.md")); err == nil {
		t.Errorf("pinned checkout must not contain a later commit's file")
	}

	if _, err := st.Install(context.Background(), GitSource(repo, "--detach"), InstallOptions{}); err == nil {
		t.Errorf("leading-dash ref must be rejected")
	}
}
