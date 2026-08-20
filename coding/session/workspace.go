package session

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/filelist"
	"github.com/latebit-io/nib/engine/openfile"
)

// Workspace methods — the agent.Workspace interface implementation that
// gives tools access to the project filesystem and open buffers. Grouped
// here so the contract is visible at one glance. Root-scoped file I/O is
// delegated to [fsroot.Root]; state (openFiles, fs, mu, langSyncer) lives
// on Session.

// SaveDirtyBuffers writes all modified (unsaved) buffers to disk and
// notifies the language service of each save. Returns the canonical
// paths of files that were successfully saved. Called by the agent
// before tool execution so it always sees the developer's latest edits.
//
// ctx is honored between save operations: if ctx fires after the Nth
// file is saved, the function returns ctx.Err() with the first N paths
// in saved so the caller can invalidate caches for completed writes
// even on cancellation. Per-file [openfile.OpenFile.Save] does
// synchronous I/O without ctx — the deadline applies at the loop
// boundary, not inside an individual write.
func (s *Session) SaveDirtyBuffers(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	// Snapshot dirty open files under read lock — Save() does I/O so we
	// don't want to hold the lock through os.WriteFile.
	type dirty struct {
		canon string
		of    *openfile.OpenFile
	}
	var toSave []dirty
	for path, of := range s.openFiles {
		if of.Modified() {
			toSave = append(toSave, dirty{canon: path, of: of})
		}
	}
	s.mu.RUnlock()

	var saved []string
	var firstErr error
	for _, d := range toSave {
		if err := ctx.Err(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		if err := d.of.Save(); err != nil {
			slog.Warn("autosave failed", "path", d.canon, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		saved = append(saved, d.canon)
		if s.langSyncer != nil {
			s.langSyncer.DidSave(d.canon)
		}
	}
	if len(saved) > 0 {
		slog.Debug("autosaved dirty buffers", "count", len(saved), "paths", saved)
	}
	return saved, firstErr
}

// ReadFile returns a file's content from disk. Called from the agent goroutine
// via Workspace — reads from disk only to avoid data races with TUI-side
// buffer mutations. The agent's FileCache is the source of truth for
// in-flight content (seeded by Run, updated after each approved edit).
// This method is only called for files not yet in the cache.
func (s *Session) ReadFile(path string) (string, error) {
	return s.fs.ReadFile(path)
}

// ListFiles returns all project files (respects .gitignore).
func (s *Session) ListFiles() ([]string, error) {
	return filelist.Walk(s.projectRoot)
}

// ListFilesAndDirs returns all project files and directories (respects .gitignore).
// Directories are returned separately so the tree can include empty directories.
func (s *Session) ListFilesAndDirs() (files []string, dirs []string, err error) {
	return filelist.WalkWithDirs(s.projectRoot)
}

// WriteFile creates a new file on disk and opens it in the session.
// Called from the agent goroutine via Workspace — the create itself is
// [fsroot.Root.WriteFile] (root-scoped, O_EXCL); mu guards the map.
func (s *Session) WriteFile(path, content string) error {
	absPath, err := s.fs.WriteFile(path, content)
	if err != nil {
		return err
	}

	// Open it in the session.
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		return fmt.Errorf("open after write %s: %w", path, err)
	}
	of := s.newOpenFile(buf)
	canon := s.CanonPath(absPath)
	s.mu.Lock()
	s.openFiles[canon] = of
	s.mu.Unlock()

	// Wire LSP sync for the newly opened file.
	s.wireBufferSync(of)

	return nil
}

// CreateDir creates a directory (and parents) within the project root.
// The path is validated via resolvePath to prevent traversal outside the root.
func (s *Session) CreateDir(path string) error {
	return s.fs.MkdirAll(path)
}

// resolvePath converts a path to a cleaned absolute path within the project
// root, rejecting lexical ("../") and symlink escapes. See [fsroot.Root.Resolve].
func (s *Session) resolvePath(path string) (string, error) {
	return s.fs.Resolve(path)
}

// CanonPath returns the cleaned absolute form of a path. Relative paths are
// resolved against the project root. This is the canonical key for the
// editors map and agent FileCache — ensures the same file is never stored
// under two keys. Satisfies agent.Workspace.
func (s *Session) CanonPath(path string) string {
	return s.fs.CanonPath(path)
}
