package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// ToolEffect classifies what the agent loop must do after a tool returns.
// Pure tools return EffectNone; tools with side effects return a specific
// effect so the agent loop handles all orchestration (events, approval flow).
type ToolEffect int

const (
	// EffectNone means no side effect — the Content is the full LLM response.
	EffectNone ToolEffect = iota
	// EffectNavigate requests editor navigation. Payload: event.AgentNavigate.
	EffectNavigate
	// EffectFileCreated signals a new file was created. Payload: string (path).
	EffectFileCreated
	// EffectEditProposed proposes an edit for approval. Payload: EditProposal.
	// The agent loop handles sending the event and blocking on approval/continue.
	EffectEditProposed
	// EffectTaskCompleted signals a plan task was marked done. Payload: nil.
	// The agent loop runs style lint + evaluator on all edited files.
	EffectTaskCompleted
)

// ToolResult is what a tool returns to the agent loop.
// Content is the string fed back to the LLM. Effect tells the loop what
// additional work to do. Payload carries effect-specific data.
type ToolResult struct {
	// Content is the text returned to the LLM as the tool result.
	Content string
	// Effect tells the agent loop what side effect to perform.
	Effect ToolEffect
	// Payload carries effect-specific data. Type depends on Effect:
	//   EffectNavigate:     event.AgentNavigate
	//   EffectFileCreated:  string (file path)
	//   EffectEditProposed: EditProposal
	Payload any
}

// EditProposal is the payload for EffectEditProposed. Contains everything
// the agent loop needs to manage the approval flow.
type EditProposal struct {
	// Edit is the proposed change sent to the frontend for approval.
	Edit event.PendingEdit
	// Path is the project-relative file path.
	Path string
	// CanonPath is the canonical absolute path (cache key).
	CanonPath string
	// ExpectedContent is what the file should contain after applying the edit.
	ExpectedContent string
}

// textResult is a convenience constructor for a pure text result with no side effect.
func textResult(content string) ToolResult {
	return ToolResult{Content: content}
}

// inProject returns true if the canonicalized path is under the project root.
// Guards against path traversal (e.g. "../../etc/passwd") and sibling-directory
// prefix confusion (e.g. "/project-secrets" vs "/project") in tool inputs.
func inProject(ws FileReader, canonPath string) bool {
	root := ws.ProjectRoot()
	if root == "" {
		return true // no root configured — allow everything
	}
	// Ensure separator-aware comparison: "/project" must match "/project/..."
	// but not "/project-secrets/...".
	prefix := strings.TrimSuffix(root, "/") + "/"
	return canonPath == root || strings.HasPrefix(canonPath, prefix)
}

// Tool defines a capability the agent can invoke during its LLM loop.
// Each tool provides its OpenAI-compatible schema and handles execution.
// Tools must be pure computations — side effects are expressed via
// ToolResult.Effect, and the agent loop handles all orchestration.
type Tool interface {
	// Definition returns the tool's function schema for the LLM.
	Definition() llm.ToolDef
	// Execute handles a tool call and returns a structured result.
	Execute(ctx context.Context, call llm.ToolCall) ToolResult
}

// Resettable is optionally implemented by tools that carry state between
// calls (e.g. retry counters). The agent calls Reset on each new run.
type Resettable interface {
	Reset()
}

// FileReader provides read-only file access to agent tools.
// Used by tools that only need to inspect project contents
// (read_file, glob, list_files, go_to_line, diagnostics, etc.).
type FileReader interface {
	// ProjectRoot returns the absolute path to the project root directory.
	ProjectRoot() string

	// ReadFile returns a file's content from disk.
	// Path is relative to the project root.
	ReadFile(path string) (string, error)

	// ListFiles returns all project files (respects .gitignore).
	// Paths are relative to the project root.
	ListFiles() ([]string, error)

	// CanonPath returns the canonical absolute form of a path.
	// Used as a consistent cache key — ensures relative and absolute
	// paths for the same file map to the same key.
	CanonPath(path string) string
}

// FileWriter provides file creation capabilities.
// Embeds FileReader because write tools always need path resolution.
type FileWriter interface {
	FileReader

	// WriteFile creates a new file on disk and opens it in the session.
	// Returns an error if the file already exists.
	WriteFile(path, content string) error
}

// ContextSet manages the developer's context set — the files the agent
// is allowed to edit. Used by the approval flow in the agent loop.
type ContextSet interface {
	// InContext returns true if the file is in the developer's context set.
	InContext(path string) bool

	// AddContext adds a file to the developer's context set.
	AddContext(path string)
}

// Workspace provides the full set of file and context operations.
// The session implements this interface. Composed from narrow interfaces
// so tools can depend only on what they need.
type Workspace interface {
	FileWriter
	ContextSet
}

// TaskTracker is an optional interface for workspaces that support
// structured task tracking via a work tree. Tools type-assert to this
// interface — it is not required for basic workspace operations.
type TaskTracker interface {
	// ActivateTask marks a task as active in the work tree and persists.
	ActivateTask(title string) error
	// CompleteTask marks a task as done in the work tree and persists.
	CompleteTask(title string) error
	// ActiveTaskPath returns the ancestry path of the current active task,
	// or empty string if no task is active.
	ActiveTaskPath() string
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

// Invalidate removes a path from the cache so the next read hits disk.
func (c *FileCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.files, path)
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
