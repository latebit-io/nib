// Package session orchestrates the developer-agent collaboration workflow.
// It owns the intent lifecycle, pending edit state, and the approve/reject/continue
// flow — domain logic that every frontend must enforce identically.
package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/filelist"
)

// Session coordinates the interaction between the developer and agent.
// Frontends read its state to render and call its methods to drive the workflow.
//
// Concurrency: the TUI goroutine calls most methods (SubmitGoal, SwitchTo,
// ApproveEdit, etc.) while the agent goroutine calls Workspace methods
// (ReadFile, WriteFile, ListFiles, CanonPath). The mu field guards the
// editors map and activeFile so both goroutines can safely access them.
type Session struct {
	// Editor points to the active editor. Updated on file switch.
	// Kept public for backward compatibility with frontends.
	Editor *editor.Editor

	agent  *agent.Agent
	Events <-chan agent.Event // frontend reads agent events from here

	// mu guards editors and activeFile for concurrent access from the
	// TUI goroutine and agent goroutine (via Workspace interface).
	mu sync.RWMutex

	// Multi-buffer state
	editors     map[string]*editor.Editor // path → editor
	activeFile  string                    // path of the active editor
	projectRoot string                    // root for file listing and path resolution

	// Intent — session-level contract between developer and agent
	CurrentIntent string   // the active goal
	IntentDone    bool     // true when intent was completed (not cleared)
	IntentHistory []string // resolved intents archived in order

	// PendingEdit is the edit currently awaiting approval (nil = none)
	PendingEdit *agent.PendingEdit

	// lastEditedFile tracks which file was approved/being edited. Used by
	// Continue to send the correct file's content even if the user switches
	// to a different file before pressing Continue.
	lastEditedFile string

	// editReviewed is set by ReviewEdit. ApproveEdit requires it.
	// This enforces the contract: every frontend must compute and present
	// the diff before approving — no blind approvals.
	editReviewed bool
}

// New creates a session in editor-only mode. Call SetAgent to enable the
// agent after construction (this breaks the circular dependency between
// session-as-workspace and agent-needing-workspace).
// The projectRoot is used for file listing and resolving relative paths.
func New(e *editor.Editor, projectRoot string) *Session {
	editors := make(map[string]*editor.Editor)
	s := &Session{
		Editor:      e,
		editors:     editors,
		projectRoot: projectRoot,
	}
	if e != nil && e.Buf.Path != "" {
		canon := s.CanonPath(e.Buf.Path)
		s.activeFile = canon
		s.editors[canon] = e
	}
	return s
}

// SetAgent wires an agent into the session. The agent is typically created
// with the session as its Workspace, so this must be called after New.
func (s *Session) SetAgent(ag *agent.Agent, events <-chan agent.Event) {
	s.agent = ag
	s.Events = events
}

// HasAgent returns true if the session has an active agent.
func (s *Session) HasAgent() bool {
	return s.agent != nil
}

// ActiveFile returns the path of the currently active file.
func (s *Session) ActiveFile() string {
	return s.activeFile
}

// ProjectRoot returns the project root path.
func (s *Session) ProjectRoot() string {
	return s.projectRoot
}

// OpenFiles returns the paths of all open editors.
func (s *Session) OpenFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files := make([]string, 0, len(s.editors))
	for path := range s.editors {
		files = append(files, path)
	}
	return files
}

// ModifiedFiles returns paths of editors with unsaved changes.
func (s *Session) ModifiedFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var modified []string
	for path, e := range s.editors {
		if e.Buf.Modified {
			modified = append(modified, path)
		}
	}
	return modified
}

// EditorForPath returns the editor for a given path, or nil if not open.
func (s *Session) EditorForPath(path string) *editor.Editor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.editors[s.CanonPath(path)]
}

// --- Workspace Implementation ---
// These methods satisfy the agent.Workspace interface, giving tools
// access to open buffers and the filesystem.

// ReadFile returns a file's content. Checks open buffers first (which may
// have unsaved changes), then falls back to disk. Called from the agent
// goroutine via Workspace — uses mu to safely access the editors map.
func (s *Session) ReadFile(path string) (string, error) {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return "", err
	}

	// Check open buffers first (may have unsaved changes).
	canon := s.CanonPath(path)
	s.mu.RLock()
	e, ok := s.editors[canon]
	s.mu.RUnlock()
	if ok {
		// Copy content under no lock — Buffer.Content() builds a new string
		// from the line array, so this is a snapshot. In the current design
		// only one goroutine mutates a given buffer at a time (the TUI owns
		// the active buffer, the agent waits on approval channels).
		return e.Buf.Content(), nil
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
		return fmt.Errorf("write %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	// Open it in the session
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		return fmt.Errorf("open after write %s: %w", path, err)
	}
	e := editor.New(buf)
	s.mu.Lock()
	s.editors[absPath] = e
	s.mu.Unlock()

	return nil
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

// --- Intent Lifecycle ---

// SubmitGoal starts the agent with a new goal.
// Archives any previous intent that was still active.
func (s *Session) SubmitGoal(goal string) {
	if !s.HasAgent() {
		return
	}
	// Archive previous intent if redirecting mid-run
	if s.CurrentIntent != "" && !s.IntentDone {
		s.ArchiveIntent()
	}
	// Clear stale pending edit from previous run — Agent.Run cancels the
	// prior run internally, so any pending approval is no longer valid.
	s.PendingEdit = nil
	s.editReviewed = false
	s.CurrentIntent = goal
	s.IntentDone = false
	s.agent.Run(s.activeFile, s.Editor.Buf.Content(), goal)
}

// ArchiveIntent marks the current intent as done.
// The intent stays visible as completed until the developer starts a new one.
func (s *Session) ArchiveIntent() {
	if s.CurrentIntent != "" {
		s.IntentDone = true
		s.IntentHistory = append(s.IntentHistory, s.CurrentIntent)
	}
}

// ClearIntent cancels the current intent without archiving.
// Used when the developer explicitly escapes/deletes the intent.
func (s *Session) ClearIntent() {
	s.CurrentIntent = ""
	s.IntentDone = false
}

// CancelAgent cancels the current agent run, clears intent, and resets pending edit.
func (s *Session) CancelAgent() {
	if s.HasAgent() {
		s.ClearIntent()
		s.agent.Cancel()
		s.PendingEdit = nil
		s.editReviewed = false
	}
}

// SwitchTo switches the active editor to a different file. If the file is
// already open, switches to it. If not, opens it from disk. Does NOT cancel
// the agent or clear intent — multi-file work continues across switches.
// Returns an error if the file cannot be opened.
func (s *Session) SwitchTo(path string) error {
	canon := s.CanonPath(path)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Already active
	if canon == s.activeFile {
		return nil
	}

	// Check if already open
	if e, ok := s.editors[canon]; ok {
		s.Editor = e
		s.activeFile = canon
		return nil
	}

	// Open from disk
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	e := editor.New(buf)
	s.editors[canon] = e
	s.Editor = e
	s.activeFile = canon
	return nil
}

// SwitchEditor replaces the current editor with a new one. Cancels any
// running agent, clears all pending state, and resets intent. Returns the
// old editor so the caller can Close() it to free resources (e.g. tree-sitter).
//
// Deprecated: Use SwitchTo for multi-buffer file switching. This method
// exists for backward compatibility with frontends that create editors externally.
func (s *Session) SwitchEditor(newEditor *editor.Editor) *editor.Editor {
	if s.HasAgent() {
		s.agent.Cancel()
	}
	s.PendingEdit = nil
	s.editReviewed = false
	s.CurrentIntent = ""
	s.IntentDone = false

	old := s.Editor
	s.mu.Lock()
	// Remove old editor from map
	if s.activeFile != "" {
		delete(s.editors, s.activeFile)
	}
	// Add new editor — only track in map if it has a path.
	s.Editor = newEditor
	if newEditor.Buf.Path != "" {
		canon := s.CanonPath(newEditor.Buf.Path)
		s.editors[canon] = newEditor
		s.activeFile = canon
	} else {
		s.activeFile = ""
	}
	s.mu.Unlock()
	return old
}

// Close frees resources for all open editors.
func (s *Session) Close() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.editors {
		e.Close()
	}
}

// --- Edit Approval Flow ---
//
// The engine enforces a two-step review contract:
//
//   1. ReviewEdit  — frontend computes and presents the diff to the developer.
//   2. ApproveEdit — frontend passes the (possibly modified) replacement text.
//
// ApproveEdit fails if ReviewEdit was not called first. This guarantees that
// every frontend — TUI, GUI, web — shows the developer what the agent proposes
// before anything is applied. No blind approvals.

// ReviewEdit computes the diff for the pending edit and marks it as reviewed.
// Frontends MUST call this and present the result before calling ApproveEdit.
// If the pending edit targets a non-active file, the session auto-switches
// to that file so the frontend renders the correct buffer.
// Returns nil if there is no pending edit or the search text has no unique match.
// Returns true for switched if the active editor changed.
func (s *Session) ReviewEdit() (diff *editor.DiffResult, switched bool) {
	if s.PendingEdit == nil {
		return nil, false
	}
	// editorForEdit may auto-open a file that isn't in the map yet.
	e := s.editorForEdit()
	if e == nil {
		return nil, false
	}
	// Auto-switch to the target file so the frontend shows the right buffer.
	// Done after editorForEdit so auto-opened files are also switched to.
	if s.PendingEdit.Path != "" {
		canon := s.CanonPath(s.PendingEdit.Path)
		s.mu.Lock()
		if canon != s.activeFile {
			s.Editor = e
			s.activeFile = canon
			switched = true
		}
		s.mu.Unlock()
	}
	diff = e.ComputeDiff(s.PendingEdit.Search, s.PendingEdit.Replace)
	if diff != nil {
		s.editReviewed = true
	}
	return diff, switched
}

// ApproveEdit applies the reviewed edit to the editor buffer.
// The frontend must provide the final search and replace text — typically
// the full affected lines from the diff, with the replacement possibly
// modified by the developer.
//
// Returns (true, "") on success, or (false, reason) on failure.
// Fails if ReviewEdit was not called first.
func (s *Session) ApproveEdit(search, replace string) (bool, string) {
	if s.PendingEdit == nil || !s.HasAgent() {
		return false, "no pending edit"
	}
	if !s.editReviewed {
		return false, "edit not reviewed — call ReviewEdit first"
	}
	e := s.editorForEdit()
	if e == nil {
		s.agent.Reject()
		s.PendingEdit = nil
		s.editReviewed = false
		return false, "file not open"
	}
	editPath := s.CanonPath(s.PendingEdit.Path)
	ok, reason := e.ApplyEdit(search, replace)
	if ok {
		s.lastEditedFile = editPath
		s.agent.Approve()
	} else {
		s.agent.Reject()
	}
	s.PendingEdit = nil
	s.editReviewed = false
	return ok, reason
}

// --- Animated Approval Flow ---
//
// For frontends that animate edits (character-by-character typing), the
// approval is split into two steps:
//
//   1. PrepareApproval — validates and returns an AnimationPlan. Clears pending
//      edit state but does NOT mutate the buffer or signal the agent.
//   2. CompleteApproval — signals the agent that the edit was applied.
//
// Between these two calls, the frontend owns the buffer mutations (delete old
// text, insert replacement char-by-char) and can animate at its own pace.

// AnimationPlan describes the edit the frontend needs to animate.
type AnimationPlan struct {
	Line    int    // buffer line where the edit starts (0-indexed)
	Col     int    // buffer col where the edit starts (0-indexed, rune)
	Search  string // text to delete from the buffer
	Replace string // text to type into the buffer
}

// PrepareApproval validates the reviewed edit and returns an AnimationPlan.
// The frontend provides the final search/replace (possibly modified in the overlay).
// Clears pending edit state but does NOT mutate the buffer or signal the agent.
//
// Returns an error if there is no pending edit, the edit was not reviewed,
// or the search text cannot be uniquely located in the buffer.
func (s *Session) PrepareApproval(search, replace string) (*AnimationPlan, error) {
	if s.PendingEdit == nil || !s.HasAgent() {
		return nil, errors.New("no pending edit")
	}
	if !s.editReviewed {
		return nil, errors.New("edit not reviewed — call ReviewEdit first")
	}
	e := s.editorForEdit()
	if e == nil {
		s.agent.Reject()
		s.PendingEdit = nil
		s.editReviewed = false
		return nil, errors.New("file not open")
	}
	loc, reason := e.LocateEdit(search)
	if loc == nil {
		s.agent.Reject()
		s.PendingEdit = nil
		s.editReviewed = false
		return nil, errors.New(reason)
	}
	s.lastEditedFile = s.CanonPath(s.PendingEdit.Path)
	s.PendingEdit = nil
	s.editReviewed = false
	return &AnimationPlan{
		Line:    loc.Line,
		Col:     loc.Col,
		Search:  search,
		Replace: replace,
	}, nil
}

// CompleteApproval signals the agent that the animated edit has been applied.
// Call this after the animation finishes (or after a yield).
func (s *Session) CompleteApproval() {
	if s.HasAgent() {
		s.agent.Approve()
	}
}

// AbortApproval rejects a prepared approval that was never completed.
// Use this when the user cancels an in-progress animation. The agent
// receives a rejection and can try a different approach — unlike
// CancelAgent which kills the entire run.
func (s *Session) AbortApproval() {
	if s.HasAgent() {
		s.agent.Reject()
	}
}

// RejectEdit rejects the pending edit and signals the agent.
func (s *Session) RejectEdit() {
	if s.PendingEdit == nil || !s.HasAgent() {
		return
	}
	s.PendingEdit = nil
	s.editReviewed = false
	s.agent.Reject()
}

// Continue signals the agent to proceed after the developer has finished editing.
// Sends the content of the file that was last edited (not necessarily the
// currently active file, in case the user switched files after approving).
func (s *Session) Continue() {
	if !s.HasAgent() {
		return
	}
	path := s.lastEditedFile
	if path == "" {
		path = s.activeFile
	}
	s.mu.RLock()
	e, ok := s.editors[path]
	s.mu.RUnlock()
	if !ok {
		e = s.Editor
		path = s.activeFile
	}
	s.agent.Continue(path, e.Buf.Content())
}

// --- Agent Event Handling ---

// HandleEvent processes an agent event and updates session state.
// Returns the event for the frontend to render.
func (s *Session) HandleEvent(ev agent.Event) {
	switch e := ev.(type) {
	case agent.EditProposedEvent:
		s.PendingEdit = &e.Edit
		s.editReviewed = false
	case agent.ErrorEvent:
		s.PendingEdit = nil
		s.editReviewed = false
		_ = e // error text is in the event for the frontend to display
	case agent.DoneEvent:
		s.PendingEdit = nil
		s.editReviewed = false
		if e.Success {
			s.ArchiveIntent()
		}
	case agent.FileCreatedEvent:
		// File is already opened by workspace.WriteFile — frontend can
		// render it in the project view or switch to it.
		_ = e
	case agent.TokenEvent, agent.StatusEvent:
		// No session state changes — frontend renders these directly
	}
}

// editorForEdit returns the editor targeted by the current pending edit.
// Falls back to the active editor if no path is set (backward compat).
// If the target file isn't open yet, auto-opens it from disk — the agent
// may have read the file via read_file (which doesn't create a buffer)
// and then proposed an edit_file on it.
func (s *Session) editorForEdit() *editor.Editor {
	if s.PendingEdit == nil {
		return s.Editor
	}
	path := s.PendingEdit.Path
	if path == "" {
		return s.Editor
	}
	canon := s.CanonPath(path)

	s.mu.RLock()
	e, ok := s.editors[canon]
	s.mu.RUnlock()
	if ok {
		return e
	}

	// Auto-open: the agent proposed an edit to a file that isn't open yet.
	absPath, err := s.resolvePath(path)
	if err != nil {
		return nil
	}
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		return nil
	}
	e = editor.New(buf)
	s.mu.Lock()
	s.editors[canon] = e
	s.mu.Unlock()
	return e
}
