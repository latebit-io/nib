package tools

import "strings"

// FileReader provides read-only file access to agent tools. Used by
// tools that only need to inspect project contents (read_file, glob,
// list_files, go_to_line, diagnostics, etc.).
type FileReader interface {
	// ProjectRoot returns the absolute path to the project root directory.
	ProjectRoot() string

	// ReadFile returns a file's content from disk. Path should be
	// relative to the project root.
	ReadFile(path string) (string, error)

	// ListFiles returns all project files (respects .gitignore). Paths
	// are relative to the project root.
	ListFiles() ([]string, error)

	// CanonPath returns the canonical absolute form of a path. Used as
	// a consistent cache key — ensures relative and absolute paths for
	// the same file map to the same key.
	CanonPath(path string) string
}

// FileWriter provides file creation capabilities. Embeds FileReader
// because write tools always need path resolution.
type FileWriter interface {
	FileReader

	// WriteFile creates a new file on disk and opens it in the session.
	// Returns an error if the file already exists.
	WriteFile(path, content string) error
}

// ContextSet manages the developer's context set — the files the agent
// is allowed to edit. Used by the approval flow.
type ContextSet interface {
	// InContext returns true if the file is in the developer's context set.
	InContext(path string) bool

	// AddContext adds a file to the developer's context set.
	AddContext(path string)
}

// Workspace provides the full set of file and context operations. The
// session implements this interface. Composed from narrow interfaces so
// tools can depend only on what they need.
type Workspace interface {
	FileWriter
	ContextSet
}

// inProject returns true if the canonicalized path is under the project
// root. Guards against path traversal (e.g. "../../etc/passwd") and
// sibling-directory prefix confusion (e.g. "/project-secrets" vs
// "/project") in tool inputs.
func inProject(ws FileReader, canonPath string) bool {
	root := ws.ProjectRoot()
	if root == "" {
		return true // no root configured — allow everything
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	return canonPath == root || strings.HasPrefix(canonPath, prefix)
}
