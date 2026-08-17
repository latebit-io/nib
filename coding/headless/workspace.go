// Package headless provides a frontend-free agent runner for autonomous
// execution. DiskWorkspace implements agent.Workspace via direct file I/O,
// and Runner drives the agent event loop without a TUI.
package headless

import (
	"github.com/latebit-io/nib/coding/fsroot"
	"github.com/latebit-io/nib/engine/filelist"
)

// DiskWorkspace implements agent.Workspace with direct file I/O.
// No buffers, no editor, no undo — files on disk are the source of truth.
// All path validation and writes go through [fsroot.Root].
type DiskWorkspace struct {
	fs fsroot.Root
}

// NewDiskWorkspace creates a workspace rooted at the given directory.
// The root must be an absolute path.
func NewDiskWorkspace(root string) *DiskWorkspace {
	return &DiskWorkspace{fs: fsroot.New(root)}
}

// ProjectRoot returns the absolute path to the project root directory.
func (w *DiskWorkspace) ProjectRoot() string {
	return w.fs.Dir()
}

// ReadFile returns a file's content from disk with a single trailing
// newline trimmed. Path may be relative to the project root or absolute.
func (w *DiskWorkspace) ReadFile(path string) (string, error) {
	return w.fs.ReadFile(path)
}

// ReadFileRaw returns the raw bytes and validated absolute path for a file.
// Unlike ReadFile, it does not trim trailing newlines — used by the runner
// to preserve the original file state when applying edits.
func (w *DiskWorkspace) ReadFileRaw(path string) ([]byte, string, error) {
	return w.fs.ReadFileRaw(path)
}

// ListFiles returns all project files (respects .gitignore).
// Paths are relative to the project root.
func (w *DiskWorkspace) ListFiles() ([]string, error) {
	return filelist.Walk(w.fs.Dir())
}

// WriteFile creates a new file on disk. Returns an error if the file
// already exists. Creates parent directories as needed.
func (w *DiskWorkspace) WriteFile(path, content string) error {
	_, err := w.fs.WriteFile(path, content)
	return err
}

// OverwriteFile writes content to an existing file on disk, replacing its
// contents. The path is validated against the project root to prevent
// traversal. Used by the headless runner to apply agent edits.
func (w *DiskWorkspace) OverwriteFile(path, content string) error {
	return w.fs.OverwriteFile(path, content)
}

// CanonPath returns the canonical absolute form of a path.
// Relative paths are resolved against the project root.
func (w *DiskWorkspace) CanonPath(path string) string {
	return w.fs.CanonPath(path)
}
