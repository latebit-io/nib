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

// Workspace provides the full set of file operations agent tools need.
// The session implements this interface. Currently aliased to
// [FileWriter] — kept as a distinct name so callers can express
// "the full workspace surface" rather than the narrower writer role.
type Workspace interface {
	FileWriter
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
