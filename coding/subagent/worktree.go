package subagent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitWorktree creates a detached git worktree of repo at HEAD and returns
// its root plus a cleanup func that removes the worktree and its staging
// parent. It is the production [worktreeFactory].
//
// The worktree lives under a fresh temp parent (outside the repo) so it
// cannot collide with the working tree. Requires repo to be a git
// repository; a non-repo or git error surfaces as an error so the spawn
// fails loudly rather than silently running unisolated.
func gitWorktree(ctx context.Context, repo string) (root string, cleanup func(), err error) {
	parent, err := os.MkdirTemp("", "nib-subagent-wt-*")
	if err != nil {
		return "", nil, fmt.Errorf("create worktree staging dir: %w", err)
	}
	// git worktree add requires the target path not to exist yet; place it
	// inside the (empty) staging parent.
	wt := filepath.Join(parent, "tree")
	if out, gerr := runGit(ctx, repo, "worktree", "add", "--detach", wt, "HEAD"); gerr != nil {
		_ = os.RemoveAll(parent)
		return "", nil, fmt.Errorf("git worktree add: %w: %s", gerr, strings.TrimSpace(out))
	}

	cleanup = func() {
		// Use a background context: the run's context may already be
		// cancelled, but the worktree must still be torn down. `git
		// worktree remove --force` drops uncommitted changes (the v1
		// isolated-run semantics — changes are not merged back).
		if out, rerr := runGit(context.Background(), repo, "worktree", "remove", "--force", wt); rerr != nil {
			slog.Warn("subagent: git worktree remove failed", "worktree", wt, "err", rerr, "out", strings.TrimSpace(out))
		}
		if rerr := os.RemoveAll(parent); rerr != nil {
			slog.Warn("subagent: worktree staging cleanup failed", "dir", parent, "err", rerr)
		}
	}
	return wt, cleanup, nil
}

// runGit runs a git subcommand in repo and returns combined output. The
// arg list is passed directly to exec (no shell), so caller-supplied
// values cannot inject commands.
func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput()
	return string(out), err
}
