// Package fsroot is the root-scoped filesystem primitive shared by the
// coding workspaces (interactive session, headless disk workspace). It
// resolves paths against a project root, rejects anything that escapes
// the root lexically or through symlinks, and provides the create-only
// and overwrite file writes both workspaces need. Workspaces layer their
// own state (open buffers, LSP sync) on top; this package holds none.
package fsroot

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Root is a cleaned absolute project root. The zero value is unusable;
// construct with [New].
type Root struct {
	dir string
}

// New returns a Root for dir. dir is cleaned but not made absolute —
// callers pass an absolute path (workspaces normalise at construction).
func New(dir string) Root {
	return Root{dir: filepath.Clean(dir)}
}

// Dir returns the cleaned root directory.
func (r Root) Dir() string { return r.dir }

// CanonPath returns the cleaned absolute form of path. Relative paths
// resolve against the root. This is the canonical key workspaces use for
// buffer maps and caches — the same file is never stored under two keys.
// CanonPath does not validate containment; use [Root.Resolve] for that.
func (r Root) CanonPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(r.dir, path))
}

// Resolve returns the cleaned absolute path for path after verifying it
// stays inside the root once symlinks are resolved. For paths that do
// not exist yet (create), the nearest existing ancestor is resolved and
// the missing tail re-joined, so `link/newdir/f.go` through an in-root
// symlink pointing outside is still rejected.
//
// Resolve is a name check; the I/O methods do not trust it. They reopen
// the root as an [os.Root] and address the file relative to it, so a
// symlink swapped in after the check still cannot escape the root.
func (r Root) Resolve(path string) (string, error) {
	abs, _, err := r.resolve(path)
	return abs, err
}

// resolve returns the cleaned absolute path and its position relative
// to the real root (the key the [os.Root] I/O methods use).
func (r Root) resolve(path string) (abs, rel string, err error) {
	abs = r.CanonPath(path)

	realRoot, err := filepath.EvalSymlinks(r.dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve project root: %w", err)
	}
	realRoot = filepath.Clean(realRoot)

	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("resolve %s: %w", path, err)
		}
		realAbs, err = resolveMissing(abs)
		if err != nil {
			return "", "", fmt.Errorf("resolve %s: %w", path, err)
		}
	}
	// filepath.Rel rather than a prefix test: a root of "/" would
	// otherwise need "//" as its prefix and reject every descendant.
	rel, err = filepath.Rel(realRoot, filepath.Clean(realAbs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q resolves outside project root", path)
	}
	return abs, rel, nil
}

// open returns the root as an [os.Root] plus the validated absolute and
// root-relative paths. Callers close the returned root.
func (r Root) open(path string) (*os.Root, string, string, error) {
	abs, rel, err := r.resolve(path)
	if err != nil {
		return nil, "", "", err
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return nil, "", "", fmt.Errorf("open project root: %w", err)
	}
	return root, abs, rel, nil
}

// resolveMissing walks up from abs to the nearest existing ancestor,
// resolves its symlinks, and re-joins the missing tail.
func resolveMissing(abs string) (string, error) {
	ancestor := filepath.Dir(abs)
	tail := []string{filepath.Base(abs)}
	for {
		realAnc, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				realAnc = filepath.Join(realAnc, tail[i])
			}
			return realAnc, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", errors.New("no existing ancestor")
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = next
	}
}

// ReadFile returns the file's content with a single trailing newline
// trimmed, matching engine buffer.NewFromFile normalisation.
func (r Root) ReadFile(path string) (string, error) {
	data, _, err := r.ReadFileRaw(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

// ReadFileRaw returns the raw bytes and the validated absolute path.
func (r Root) ReadFileRaw(path string) ([]byte, string, error) {
	root, abs, rel, err := r.open(path)
	if err != nil {
		return nil, "", err
	}
	defer closeRoot(root)
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	return data, abs, nil
}

// WriteFile creates a new file, creating parent directories as needed,
// and returns its absolute path. Fails if the file already exists
// (O_EXCL — no Stat/Write TOCTOU). A failed write is rolled back so a
// retry does not hit "already exists".
func (r Root) WriteFile(path, content string) (string, error) {
	root, abs, rel, err := r.open(path)
	if err != nil {
		return "", err
	}
	defer closeRoot(root)
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create directory %s: %w", filepath.Dir(abs), err)
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("file already exists: %s", path)
		}
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr == nil && closeErr == nil {
		return abs, nil
	}
	if rmErr := root.Remove(rel); rmErr != nil {
		slog.Warn("fsroot: rollback of partial write failed", "path", abs, "err", rmErr)
	}
	if writeErr != nil {
		return "", fmt.Errorf("write %s: %w", path, writeErr)
	}
	return "", fmt.Errorf("close %s: %w", path, closeErr)
}

// OverwriteFile replaces the content of a file inside the root.
func (r Root) OverwriteFile(path, content string) error {
	root, _, rel, err := r.open(path)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	if err := root.WriteFile(rel, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// MkdirAll creates a directory (and parents) inside the root.
func (r Root) MkdirAll(path string) error {
	root, _, rel, err := r.open(path)
	if err != nil {
		return err
	}
	defer closeRoot(root)
	if rel == "." {
		return nil
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	return nil
}

// closeRoot closes a directory handle; the close of a read-only fd
// carries no data, so a failure is only worth a log line.
func closeRoot(root *os.Root) {
	if err := root.Close(); err != nil {
		slog.Warn("fsroot: close project root", "err", err)
	}
}
