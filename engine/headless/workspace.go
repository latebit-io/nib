// Package headless provides a frontend-free agent runner for autonomous
// execution. DiskWorkspace implements agent.Workspace via direct file I/O,
// and Runner drives the agent event loop without a TUI.
package headless

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/latebit-io/junto/engine/filelist"
)

// DiskWorkspace implements agent.Workspace with direct file I/O.
// No buffers, no editor, no undo — files on disk are the source of truth.
type DiskWorkspace struct {
	root string

	mu      sync.Mutex
	context map[string]bool // tracks files the agent has touched
}

// NewDiskWorkspace creates a workspace rooted at the given directory.
// The root must be an absolute path.
func NewDiskWorkspace(root string) *DiskWorkspace {
	return &DiskWorkspace{
		root:    filepath.Clean(root),
		context: make(map[string]bool),
	}
}

// ProjectRoot returns the absolute path to the project root directory.
func (w *DiskWorkspace) ProjectRoot() string {
	return w.root
}

// ReadFile returns a file's content from disk.
// Path may be relative to the project root or absolute.
func (w *DiskWorkspace) ReadFile(path string) (string, error) {
	abs, err := w.resolvePath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	// Normalize: trim a single trailing newline (matches buffer.NewFromFile).
	content := strings.TrimSuffix(string(data), "\n")
	return content, nil
}

// ReadFileRaw returns the raw bytes and validated absolute path for a file.
// Unlike ReadFile, it does not trim trailing newlines — used by the runner
// to preserve the original file state when applying edits.
func (w *DiskWorkspace) ReadFileRaw(path string) ([]byte, string, error) {
	abs, err := w.resolvePath(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	return data, abs, nil
}

// ListFiles returns all project files (respects .gitignore).
// Paths are relative to the project root.
func (w *DiskWorkspace) ListFiles() ([]string, error) {
	return filelist.Walk(w.root)
}

// WriteFile creates a new file on disk. Returns an error if the file
// already exists. Creates parent directories as needed.
func (w *DiskWorkspace) WriteFile(path, content string) error {
	abs, err := w.resolvePath(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	// Atomic create — O_EXCL fails if the file already exists.
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("file already exists: %s", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(abs) // best-effort rollback
		return fmt.Errorf("write %s: %w", path, writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(abs) // best-effort rollback
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	w.mu.Lock()
	w.context[w.CanonPath(path)] = true
	w.mu.Unlock()
	return nil
}

// OverwriteFile writes content to an existing file on disk, replacing its
// contents. The path is validated against the project root to prevent
// traversal. Used by the headless runner to apply agent edits.
func (w *DiskWorkspace) OverwriteFile(path, content string) error {
	abs, err := w.resolvePath(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// CanonPath returns the canonical absolute form of a path.
// Relative paths are resolved against the project root.
func (w *DiskWorkspace) CanonPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(w.root, path))
}

// InContext always returns true — headless mode has no context restrictions.
// The agent can edit any file in the project.
func (w *DiskWorkspace) InContext(_ string) bool {
	return true
}

// AddContext records that a file has been touched by the agent.
func (w *DiskWorkspace) AddContext(path string) {
	w.mu.Lock()
	w.context[w.CanonPath(path)] = true
	w.mu.Unlock()
}

// TouchedFiles returns the set of files the agent has added to context.
// Useful for reporting which files were modified during a run.
func (w *DiskWorkspace) TouchedFiles() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	files := make([]string, 0, len(w.context))
	for path := range w.context {
		files = append(files, path)
	}
	return files
}

// resolvePath resolves a path relative to the project root and validates
// it does not escape the root via symlinks or traversal.
func (w *DiskWorkspace) resolvePath(path string) (string, error) {
	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		abs = filepath.Clean(filepath.Join(w.root, path))
	}

	realRoot, err := filepath.EvalSymlinks(w.root)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	realRoot = filepath.Clean(realRoot)

	// Try resolving the full path first (catches leaf symlinks).
	// If the file doesn't exist yet (WriteFile), fall back to resolving
	// the parent directory and joining the base name.
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %s: %w", path, err)
		}
		// File doesn't exist — walk up to the nearest existing ancestor
		// and resolve from there. This handles WriteFile where intermediate
		// dirs will be created.
		ancestor := filepath.Dir(abs)
		var tail []string
		for {
			realAnc, ancErr := filepath.EvalSymlinks(ancestor)
			if ancErr == nil {
				// Rebuild the full resolved path from the resolved ancestor.
				for i := len(tail) - 1; i >= 0; i-- {
					realAnc = filepath.Join(realAnc, tail[i])
				}
				realAbs = filepath.Join(realAnc, filepath.Base(abs))
				break
			}
			if !os.IsNotExist(ancErr) {
				return "", fmt.Errorf("resolve %s: %w", path, ancErr)
			}
			// This ancestor doesn't exist either — go up one level.
			tail = append(tail, filepath.Base(ancestor))
			next := filepath.Dir(ancestor)
			if next == ancestor {
				// Reached filesystem root without finding an existing dir.
				return "", fmt.Errorf("resolve %s: no existing ancestor", path)
			}
			ancestor = next
		}
	}

	if realAbs != realRoot && !strings.HasPrefix(realAbs, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside project root", path)
	}
	return abs, nil
}
