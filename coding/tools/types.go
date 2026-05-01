// Package tools holds the application-layer tool implementations the
// the agent dispatches against. Each file in this package is a
// agent tool (read_file, edit_file, …) that satisfies the
// generic [agent.Tool] interface defined in the upstream
// `agent` module.
//
// Tools own their side effects directly: they are constructed with
// narrow collaborator interfaces (Approver, Navigator, FileCreator,
// TaskReviewer) that the application layer in `coding/agent` provides.
// The agent loop never inspects the tool's return shape beyond passing
// the [agent.ToolResult] body to the LLM and the IsError flag to the
// frontend.
//
// This package is the home of the workspace-side contracts (FileReader,
// FileWriter, Workspace, ContextSet, TaskReader, TaskMutator,
// TaskTracker), the [FileCache] used by file-touching tools, and the
// [EditProposal] payload that flows from edit/replace tools through
// [Approver] to the approval pipeline.
package tools

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/coding/event"
)

// Tool aliases the upstream generic [agent.Tool] interface so each
// tool in this package can name it unqualified. Tools satisfy the
// interface implicitly via Definition / Execute methods.
type Tool = agent.Tool

// ToolResult aliases the upstream generic [agent.ToolResult]. The
// shape is deliberately minimal: a string body and an error flag.
type ToolResult = agent.ToolResult

// textResult builds a successful tool result whose body is the given
// string. Convenience constructor used by every tool.
func textResult(content string) ToolResult {
	return ToolResult{Content: content}
}

// errorResult builds a tool result whose body is the given error
// message and whose IsError flag is set so frontends can render it
// distinctively.
func errorResult(content string) ToolResult {
	return ToolResult{Content: content, IsError: true}
}

// lspTimeout is the default timeout for LSP requests made by tools.
const lspTimeout = 10 * time.Second

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

// TaskReader exposes read-only views over the project's task tree.
type TaskReader interface {
	// ActiveTaskPath returns the ancestry path of the current active
	// task, or empty string if no task is active.
	ActiveTaskPath() string

	// WorkTreeLoaded reports whether the session currently holds a
	// parsed work tree. False means either /project.md does not exist
	// yet or the initial fetch failed (e.g. demarkus unreachable).
	WorkTreeLoaded() bool

	// NextPendingTask returns the title of the first leaf task in
	// document order with status TaskPending, or "" when none exists.
	NextPendingTask() string
}

// TaskMutator covers the write-side operations on the project task
// tree. Tool implementations consume the narrowest mutation surface
// they need (e.g. project_init only requires InitProject).
type TaskMutator interface {
	// ActivateTask marks a task as active in the work tree and persists.
	ActivateTask(title string) error

	// CompleteTask marks a task as done in the work tree and persists.
	CompleteTask(title string) error

	// AddTask appends a new pending task under the given phase and
	// feature, creating the feature if absent, then persists. If link
	// is non-empty, it is appended to the task title as a markdown link.
	AddTask(phase, feature, task, link string) error

	// InitProject ensures /project.md exists with the given project
	// name and h1-level phases, then reloads the work tree so subsequent
	// task operations succeed without manual memory bootstrapping.
	// Idempotent — never overwrites an existing plan.
	InitProject(name string, phases []string) error
}

// TaskTracker is the union surface used at the workspace boundary.
// Tool authors should prefer the narrower [TaskReader] / [TaskMutator]
// interfaces when their tool only needs one half of the contract.
type TaskTracker interface {
	TaskReader
	TaskMutator
}

// EditProposal carries the data an edit_file or replace_file tool
// hands to the [Approver] for the full validation+approval+continue
// dance. The application layer's Approver implementation owns the
// channels and event delivery; the tool just builds and submits.
type EditProposal struct {
	// Edit is the proposed change sent to the frontend for approval.
	Edit event.PendingEdit

	// Path is the project-relative file path.
	Path string

	// CanonPath is the canonical absolute path (cache key).
	CanonPath string

	// ExpectedContent is what the file should contain after applying
	// the edit. The orchestrator seeds the cache with this value
	// post-approval so subsequent tool reads see the post-edit state.
	ExpectedContent string
}

// FileCache is a concurrency-safe cache of file contents. The agent
// maintains its own view of file state, updated through tool reads
// and post-approval seeding, to avoid races with user edits.
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

// LoadOrRead returns cached content for canon, or reads from disk via
// reader and populates the cache on miss. The reader is zero-arg so
// callers capture the read path in a closure — this prevents accidental
// conflation of the cache key (canonical absolute path) with the
// reader's input path contract.
func (c *FileCache) LoadOrRead(canon string, reader func() (string, error)) (string, error) {
	if content, ok := c.Get(canon); ok {
		slog.Debug("file cache: hit", "path", canon, "content_len", len(content))
		return content, nil
	}
	content, err := reader()
	if err != nil {
		return "", err
	}
	slog.Debug("file cache: read from disk", "path", canon, "content_len", len(content))
	c.Set(canon, content)
	return content, nil
}

// Reset clears the cache and seeds it with the given file.
func (c *FileCache) Reset(path, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = map[string]string{path: content}
}

// posArgs holds the JSON-decoded position arguments shared by LSP tools
// (go_to_definition, find_references).
type posArgs struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// validatePosArgs returns an error string if the position arguments are
// invalid, or empty string on success.
func validatePosArgs(args posArgs) string {
	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Line < 1 {
		return "Error: line must be >= 1 (1-indexed)"
	}
	if args.Col < 0 {
		return "Error: col must be >= 0"
	}
	return ""
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

// Resettable is optionally implemented by tools that carry state
// between calls (e.g. retry counters). The agent calls Reset on each
// new run.
type Resettable interface {
	Reset()
}
