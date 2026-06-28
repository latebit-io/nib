package pluginstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fetcher materializes a [Source] into dest (an empty directory the
// caller owns) and returns the resolved version pin — a commit SHA for
// git sources, empty for local ones. Implementations must not touch any
// path outside dest.
//
// Splitting fetch behind an interface keeps the store's install/sync
// orchestration testable without the network: tests inject a local
// fetcher (or a fake) while production wires [DefaultFetcher].
type Fetcher interface {
	Fetch(ctx context.Context, src Source, dest string) (pin string, err error)
}

// DefaultFetcher dispatches by [Source.Type] to the built-in fetchers:
// local-filesystem copy and git clone (covering github shorthand). npm
// is not yet supported (M5).
type DefaultFetcher struct{}

// Fetch implements [Fetcher].
func (DefaultFetcher) Fetch(ctx context.Context, src Source, dest string) (string, error) {
	if err := src.Validate(); err != nil {
		return "", err
	}
	switch src.Type {
	case SourceLocal:
		return "", copyTree(src.Path, dest)
	case SourceGit, SourceGitHub:
		return gitFetch(ctx, src, dest)
	case SourceNPM:
		return "", fmt.Errorf("pluginstore: npm sources are not supported yet")
	default:
		return "", fmt.Errorf("pluginstore: cannot fetch source type %q", src.Type)
	}
}

// gitFetch clones the source repository into dest at the requested ref
// and returns the checked-out commit SHA. A subdir source clones the
// whole repo (git has no first-class sparse single-dir clone that is
// also portable) and the caller scopes to the subdir afterward.
func gitFetch(ctx context.Context, src Source, dest string) (string, error) {
	url, err := src.gitURL()
	if err != nil {
		return "", err
	}

	// Clone shallow to keep imports fast; a specific ref needs its own
	// fetch since --depth 1 of the default branch may not contain it.
	args := []string{"clone", "--depth", "1"}
	if src.Ref != "" {
		args = append(args, "--branch", src.Ref)
	}
	args = append(args, url, dest)
	if out, err := runGit(ctx, "", args...); err != nil {
		return "", fmt.Errorf("pluginstore: git clone %s: %w: %s", url, err, out)
	}

	sha, err := runGit(ctx, dest, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("pluginstore: resolve HEAD in %s: %w: %s", dest, err, sha)
	}
	return strings.TrimSpace(sha), nil
}

// runGit runs a git subcommand, optionally in dir, returning combined
// output. Kept tiny and shell-free (no string interpolation into a
// shell) so plugin-supplied refs cannot inject commands.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// copyTree recursively copies the directory tree rooted at src into dst,
// preserving file modes. dst is created if absent. Symlinks are copied
// as regular files (their target content) to avoid escaping the store;
// this is sufficient for plugin source trees, which are plain files.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("pluginstore: stat %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("pluginstore: local source %s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, fi.Mode().Perm())
	})
}

// copyFile copies a single regular file, creating parent directories.
func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read-only handle; close error is not actionable
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() // surface the copy error, not the close
		return err
	}
	return out.Close()
}
