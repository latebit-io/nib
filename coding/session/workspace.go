package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/filelist"
	"github.com/latebit-io/nib/engine/openfile"
)

// Workspace methods — the agent.Workspace interface implementation that
// gives tools access to the project filesystem and open buffers. Grouped
// here so the contract is visible at one glance. State (openFiles,
// contextSet, projectRoot, mu, langSyncer) lives on Session.

// SaveDirtyBuffers writes all modified (unsaved) buffers to disk and
// notifies the language service of each save. Returns the canonical
// paths of files that were successfully saved. Called by the agent
// before tool execution so it always sees the developer's latest edits.
func (s *Session) SaveDirtyBuffers() ([]string, error) {
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
	absPath, err := s.resolvePath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	// Normalize to match buffer.NewFromFile: trim a single trailing newline.
	content := strings.TrimSuffix(string(data), "\n")
	return content, nil
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
// Called from the agent goroutine via Workspace — uses mu for map access
// and O_CREATE|O_EXCL for atomic existence check + create.
func (s *Session) WriteFile(path, content string) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}

	// Create parent directories
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	// Atomic create — O_EXCL fails if the file already exists, avoiding
	// the TOCTOU race between Stat and WriteFile.
	f, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("file already exists: %s", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(absPath) // best-effort rollback so retry doesn't hit "already exists"
		return fmt.Errorf("write %s: %w", path, writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(absPath) // best-effort rollback
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	// Open it in the session and auto-add to context
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		return fmt.Errorf("open after write %s: %w", path, err)
	}
	of := s.newOpenFile(buf)
	canon := s.CanonPath(absPath)
	addToContext := !s.isProjectMeta(canon)
	s.mu.Lock()
	s.openFiles[canon] = of
	if addToContext {
		s.contextSet[canon] = true
	}
	s.mu.Unlock()

	// Wire LSP sync for the newly opened file.
	s.wireBufferSync(of)

	if addToContext {
		s.saveContext()
	}

	return nil
}

// CreateDir creates a directory (and parents) within the project root.
// The path is validated via resolvePath to prevent traversal outside the root.
func (s *Session) CreateDir(path string) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(absPath, 0o755)
}

// resolvePath converts a path to a cleaned absolute path within the project
// root. Accepts both relative paths (resolved against root) and absolute paths
// (validated to be within root). Returns an error if the resolved path
// escapes the project root via lexical traversal ("../") or symlinks.
func (s *Session) resolvePath(path string) (string, error) {
	abs := s.CanonPath(path)
	root := filepath.Clean(s.projectRoot)

	// Lexical check first (catches "../" before touching the filesystem).
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes project root", path)
	}

	// Resolve symlinks to catch links that point outside the root.
	// For new files that don't exist yet, evaluate the parent directory.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// File may not exist yet (write_file). Check the parent instead.
		parentDir := filepath.Dir(abs)
		realParent, dirErr := filepath.EvalSymlinks(parentDir)
		if dirErr != nil {
			// Parent doesn't exist either — will fail at create time, allow it.
			return abs, nil
		}
		realParent = filepath.Clean(realParent)
		if realParent != realRoot && !strings.HasPrefix(realParent, realRoot+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q resolves outside project root via symlink", path)
		}
		return abs, nil
	}
	realAbs = filepath.Clean(realAbs)
	realRoot = filepath.Clean(realRoot)
	if realAbs != realRoot && !strings.HasPrefix(realAbs, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside project root via symlink", path)
	}
	return abs, nil
}

// CanonPath returns the cleaned absolute form of a path. Relative paths are
// resolved against the project root. This is the canonical key for the
// editors map and agent FileCache — ensures the same file is never stored
// under two keys. Satisfies agent.Workspace.
func (s *Session) CanonPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(s.projectRoot, path))
}
