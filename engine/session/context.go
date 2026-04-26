package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The context set defines which files the agent is allowed to edit.
// Files are auto-added when the developer opens them or when the agent
// creates new files. The developer can also add/remove files explicitly.
//
// State lives on Session (contextSet, contextSaveMu) — these methods
// form a focused subsystem that operates on those two fields plus
// projectRoot/CanonPath/resolvePath. Kept on Session rather than
// extracted to a sub-type so the existing public API
// (s.AddContext, s.InContext, s.ContextFiles) is unchanged for
// frontends; the file boundary is the boundary that matters for SRP.

// AddContext adds a file to the context set. The path is canonicalized
// and validated to be within the project root.
func (s *Session) AddContext(path string) {
	if _, err := s.resolvePath(path); err != nil {
		slog.Warn("AddContext: path rejected", "path", path, "err", err)
		return
	}
	canon := s.CanonPath(path)
	s.mu.Lock()
	s.contextSet[canon] = true
	s.mu.Unlock()
	s.saveContext()
}

// RemoveContext removes a file from the context set.
func (s *Session) RemoveContext(path string) {
	canon := s.CanonPath(path)
	s.mu.Lock()
	delete(s.contextSet, canon)
	s.mu.Unlock()
	s.saveContext()
}

// InContext returns true if the path is in the context set.
// Satisfies agent.Workspace.
func (s *Session) InContext(path string) bool {
	canon := s.CanonPath(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contextSet[canon]
}

// ContextFiles returns the context set as sorted relative paths.
func (s *Session) ContextFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files := make([]string, 0, len(s.contextSet))
	for path := range s.contextSet {
		rel, err := filepath.Rel(s.projectRoot, path)
		if err != nil {
			rel = path
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files
}

// contextPath returns the path to .project/context.md.
func (s *Session) contextPath() string {
	return filepath.Join(s.projectRoot, ".project", "context.md")
}

// loadContext reads .project/context.md and populates the context set.
// Silently does nothing if the file doesn't exist.
func (s *Session) loadContext() {
	data, err := os.ReadFile(s.contextPath())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("loadContext: read context.md", "err", err)
		}
		return
	}
	// Parse paths and canonicalize outside the lock (CanonPath is pure
	// computation today, but keeping I/O-adjacent work outside locks
	// avoids future deadlock risk if CanonPath ever changes).
	var paths []string
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if rel == "" {
			continue
		}
		if _, err := s.resolvePath(rel); err != nil {
			slog.Warn("loadContext: skipping invalid path", "path", rel, "err", err)
			continue
		}
		canon := s.CanonPath(rel)
		// Skip auto-managed project metadata so the persisted context.md
		// can't make .project/* entries sticky across restarts (which
		// would defeat the isProjectMeta boundary used elsewhere).
		if s.isProjectMeta(canon) {
			continue
		}
		paths = append(paths, canon)
	}
	s.mu.Lock()
	for _, p := range paths {
		s.contextSet[p] = true
	}
	s.mu.Unlock()
}

// isProjectMeta returns true if the canonical path is inside .project/.
// These files are project metadata, not source — they should not be
// auto-added to the context set.
func (s *Session) isProjectMeta(canon string) bool {
	prefix := filepath.Join(s.projectRoot, ".project") + string(filepath.Separator)
	return strings.HasPrefix(canon, prefix)
}

// saveContext writes the context set to .project/context.md.
func (s *Session) saveContext() {
	if s.projectRoot == "" {
		return
	}

	s.contextSaveMu.Lock()
	defer s.contextSaveMu.Unlock()

	dir := filepath.Join(s.projectRoot, ".project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("saveContext: create .project dir", "err", err)
		return
	}

	files := s.ContextFiles()
	var buf strings.Builder
	buf.WriteString("# Context\n\n")
	for _, f := range files {
		fmt.Fprintf(&buf, "- %s\n", f)
	}

	if err := os.WriteFile(s.contextPath(), []byte(buf.String()), 0o644); err != nil {
		slog.Warn("saveContext: write context.md", "err", err)
	}
}
