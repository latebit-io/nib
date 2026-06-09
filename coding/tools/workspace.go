package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

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

// workTreeFilename is the basename of the agent's work tree document.
// The authoritative work tree lives in demarkus at "/project.md", not
// on the filesystem — a literal project.md at the project root would
// shadow the demarkus copy and silently diverge from the gate's view
// of the world. [collidesWithWorkTree] rejects such paths before any
// file mutation tool writes to disk.
const workTreeFilename = "project.md"

// collidesWithWorkTree reports whether canonPath points to a literal
// `project.md` at the project root. The basename match is
// case-insensitive so the guard holds on case-preserving (macOS,
// Windows) filesystems where `Project.MD` and `project.md` refer to
// the same file.
//
// Sub-paths (e.g. `docs/project.md`, `kit/memory/project.md`) are
// allowed — those are ordinary project files, not the work tree.
func collidesWithWorkTree(ws FileReader, canonPath string) bool {
	root := ws.ProjectRoot()
	if root == "" {
		return false
	}
	clean := filepath.Clean(canonPath)
	if !strings.EqualFold(filepath.Base(clean), workTreeFilename) {
		return false
	}
	return filepath.Dir(clean) == filepath.Clean(root)
}

// workTreeCollisionError is the standard rejection message for paths
// that collide with the work tree filename at the project root.
// Names the proper tools so the LLM can recover in the next turn
// instead of retrying the same misdirected write.
func workTreeCollisionError(path string) ToolResult {
	return errorResult(fmt.Sprintf(
		"Error: %q would shadow the agent's work tree. /project.md lives in demarkus, not the filesystem. "+
			"Use project_init to bootstrap it, update_task (or the bundled activate_task/complete_task fields) to change task status, "+
			"and project_task_add to append new tasks.",
		path,
	))
}
