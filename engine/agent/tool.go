package agent

import (
	"context"
	"sync"

	"github.com/latebit-io/junto/engine/llm"
)

// Tool defines a capability the agent can invoke during its LLM loop.
// Each tool provides its OpenAI-compatible schema and handles execution.
type Tool interface {
	// Definition returns the tool's function schema for the LLM.
	Definition() llm.ToolDef
	// Execute handles a tool call and returns the result string for the LLM.
	Execute(ctx context.Context, call llm.ToolCall) string
}

// Resettable is optionally implemented by tools that carry state between
// calls (e.g. retry counters). The agent calls Reset on each new run.
type Resettable interface {
	Reset()
}

// Workspace provides file operations to agent tools.
// The session implements this interface, giving tools access to
// open buffers (for modified-but-unsaved content) and the filesystem.
type Workspace interface {
	// ReadFile returns a file's content from disk.
	// Path is relative to the project root.
	ReadFile(path string) (string, error)

	// ListFiles returns all project files (respects .gitignore).
	// Paths are relative to the project root.
	ListFiles() ([]string, error)

	// WriteFile creates a new file on disk and opens it in the session.
	// Returns an error if the file already exists.
	WriteFile(path, content string) error

	// CanonPath returns the canonical absolute form of a path.
	// Used as a consistent cache key — ensures relative and absolute
	// paths for the same file map to the same key.
	CanonPath(path string) string

	// InContext returns true if the file is in the developer's context set.
	InContext(path string) bool

	// AddContext adds a file to the developer's context set.
	AddContext(path string)
}

// FileCache is a concurrency-safe cache of file contents. The agent
// maintains its own view of file state, updated only through explicit
// channels (Run, Continue), to avoid races with user edits.
type FileCache struct {
	mu    sync.Mutex
	files map[string]string
}

// NewFileCache creates an empty file cache.
func NewFileCache() *FileCache {
	return &FileCache{files: make(map[string]string)}
}

// Get returns the cached content for a path, if present.
func (c *FileCache) Get(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.files[path]
	return content, ok
}

// Set updates the cached content for a path.
func (c *FileCache) Set(path, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[path] = content
}

// Reset clears the cache and seeds it with the given file.
func (c *FileCache) Reset(path, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = map[string]string{path: content}
}
