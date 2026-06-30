package subagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestGitWorktree_Lifecycle exercises the real git worktree create/cleanup
// against a temp repo: the worktree exists with the repo's content, and
// cleanup removes both the worktree and its staging parent.
func TestGitWorktree_Lifecycle(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.email", "t@example.com")
	git(t, repo, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "init")

	root, cleanup, err := gitWorktree(context.Background(), repo)
	if err != nil {
		t.Fatalf("gitWorktree: %v", err)
	}
	// The worktree has the committed content and is a distinct directory.
	if root == repo {
		t.Errorf("worktree root must differ from the repo")
	}
	data, err := os.ReadFile(filepath.Join(root, "file.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("worktree content wrong: %q %v", data, err)
	}
	// An edit in the worktree does not touch the parent repo.
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, _ := os.ReadFile(filepath.Join(repo, "file.txt"))
	if string(parent) != "hello" {
		t.Errorf("parent repo must be unaffected by worktree edit, got %q", parent)
	}

	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after cleanup, stat err=%v", err)
	}
}

func TestGitWorktree_NonRepoErrors(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, _, err := gitWorktree(context.Background(), t.TempDir()); err == nil {
		t.Errorf("expected error creating a worktree of a non-git directory")
	}
}
