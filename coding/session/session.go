// Package session orchestrates the developer-agent collaboration workflow.
// It owns the intent lifecycle, pending edit state, and the approve/reject
// flow — domain logic that every frontend must enforce identically.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/latebit-io/junto/engine/search"
	"time"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/capture"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/filelist"
	"github.com/latebit-io/junto/engine/lang"
)

// agentLifecycle is the subset of agent operations that start, extend, or
// terminate a run. Split from signal methods so each interface stays focused.
type agentLifecycle interface {
	RunWithMode(ctx context.Context, fileName, fileContent, goal string, contextFiles []string, mode event.Mode)
	Reply(ctx context.Context, input string) bool
	Cancel()
}

// agentSignals is the subset of agent operations that deliver developer
// responses during an active run — edit approve / reject. All are
// non-blocking sends. Approve carries the post-apply buffer content
// so the orchestrator seeds its file cache from the truth, not the
// agent's predicted ExpectedContent (which can diverge when the
// developer modifies the replacement text in the diff overlay).
type agentSignals interface {
	Approve(content string)
	Reject()
}

// agentPort is the narrow interface Session needs from an agent implementation.
// Defined here (not in the agent package) so Session depends on an abstraction,
// not a concrete type (DIP). Composed from agentLifecycle and agentSignals to
// keep each responsibility focused (ISP).
type agentPort interface {
	agentLifecycle
	agentSignals
}

// Session coordinates the interaction between the developer and agent.
// Frontends read its state to render and call its methods to drive the workflow.
//
// Concurrency: the TUI goroutine calls most methods (SubmitGoal, SwitchTo,
// ApproveEdit, etc.) while the agent goroutine calls Workspace methods
// (ReadFile, WriteFile, ListFiles, CanonPath). The mu field guards the
// editors map and activeFile so both goroutines can safely access them.
type Session struct {
	// activeEditor points to the currently-focused editor. Updated on file
	// switch. Callers outside this package read via ActiveEditor(); direct
	// field access is reserved for session internals so the workflow-enforcing
	// methods remain the only mutation path.
	activeEditor *editor.Editor

	// ctx is the application-level context. Agent runs derive a child
	// context so they are cancelled when the application shuts down.
	ctx context.Context

	agent  agentPort
	events <-chan event.Event // frontend reads engine events from here

	// mu guards editors and activeFile for concurrent access from the
	// TUI goroutine and agent goroutine (via Workspace interface).
	mu sync.RWMutex

	// Multi-buffer state
	editors     map[string]*editor.Editor // path → editor
	activeFile  string                    // path of the active editor
	projectRoot string                    // root for file listing and path resolution

	// Context set — files the agent is allowed to edit.
	// Canonical absolute paths as keys. Guarded by mu.
	contextSet map[string]bool

	// contextSaveMu serializes writes to .project/context.md.
	// Separate from mu to avoid deadlock (saveContext calls ContextFiles
	// which acquires mu.RLock).
	contextSaveMu sync.Mutex

	// Intent — session-level contract between developer and agent
	currentIntent string   // the active goal
	intentDone    bool     // true when intent was completed (not cleared)
	intentHistory []string // resolved intents archived in order

	// phase tracks the current workflow phase (planning vs execution).
	phase Phase

	// pendingEdit is the edit currently awaiting approval (nil = none)
	pendingEdit *event.PendingEdit

	// lastEditedFile tracks the canonical path of the file most recently
	// edited via the approval flow. Used to attribute the "modified"
	// badge in the project pane.
	lastEditedFile string

	// stagedEditFile is set by PrepareApproval and promoted to lastEditedFile
	// by CompleteApproval. Cleared by AbortApproval. This ensures
	// lastEditedFile only reflects edits that actually landed.
	stagedEditFile string

	// editReviewed is set by ReviewEdit. ApproveEdit requires it.
	// This enforces the contract: every frontend must compute and present
	// the diff before approving — no blind approvals.
	editReviewed bool

	// modifiedFiles tracks files changed by agent edits this session.
	// Canonical absolute paths as keys. Guarded by mu.
	modifiedFiles map[string]bool

	// sink is the capture port for session events (intent, proposal,
	// accepted, rejected, validator). Initialised to capture.NoopSink in
	// New so every call site can dispatch unconditionally. Swapped for a
	// live adapter via SetEventSink at the composition root.
	sink capture.SessionEventSink

	// sessionID groups capture events emitted during this Junto process
	// lifetime. Generated once in New; the demarkus adapter uses it as
	// the per-process document identifier.
	sessionID string

	// pendingProposedReplace stores the agent's originally-proposed
	// replacement text for the active proposal. Compared to the applied
	// replacement in ApproveEdit so the "accepted" capture event can
	// include the pre-modification text when the developer edited the
	// proposal in the diff overlay.
	pendingProposedReplace string

	// pendingApproval carries the edit identity and applied search/replace
	// from PrepareApproval to CompleteApproval so the staged-flow path can
	// emit the same "accepted" capture event as ApproveEdit. Cleared by
	// CompleteApproval and AbortApproval.
	pendingApproval *stagedApproval

	// langSyncer is the language service port (optional, nil when no LSP).
	// Session depends on the interface, never on lsp.Manager directly (DIP).
	// Set once via SetLanguageService before the TUI starts — effectively
	// immutable after initialization. Read without lock is safe.
	langSyncer lang.DocumentSyncer

	// wiredEditors tracks which editors have had LSP sync wired (by canonical path).
	// Maps to the previous OnChange handler that was installed before wiring,
	// so unwireBufferSync can restore it. nil func() means no previous handler.
	// Guarded by mu.
	wiredEditors map[string]func()

	// navStack tracks cursor positions for go-back navigation after go-to-definition.
	// Each entry records where the cursor was before the jump.
	navStack []lang.Location

	// completionMu serializes overlay completion requests to prevent
	// DidChange interleaving when multiple requests race.
	completionMu sync.Mutex

	// workTree manages the structured project hierarchy loaded from demarkus
	// memory. Owns its own lock — see work_tree.go.
	workTree WorkTreeManager

	// distributedMemory lists MCP server names classified as shared/team memory.
	// Set once during startup, read by frontends for status display.
	distributedMemory []string

	// llmModel is the full model ID (e.g. "google/gemini-2.5-flash").
	// Set during startup and updated on model switch.
	llmModel string

	// llmProfile is the active profile name.
	llmProfile string

	// switchModel is the injected model-switching function. It resolves the
	// profile, wires auth, creates a new provider, and hot-swaps it on the
	// agent. Set via SetModelSwitcher at startup. Returns the display model
	// name and any error. Nil means model switching is not available.
	switchModel func(profile, modelID string) (displayModel string, err error)

	// highlighterFactory produces a [editor.Highlighter] for a given file
	// path. Frontends that render source (TUI) install the real factory via
	// [SetHighlighterFactory]; headless binaries (junto-agent) leave it nil
	// so tree-sitter grammar blobs are never linked in. Guarded by mu.
	highlighterFactory editor.HighlighterFactory

	// highlighterFactoryGen increments on every [SetHighlighterFactory]
	// call. decorateEditor reads (factory, gen) under RLock, installs the
	// highlighter outside the lock, then re-checks gen — if it changed, a
	// concurrent factory swap happened and decoration retries with the new
	// factory. This makes concurrent decoration linearizable with swaps.
	highlighterFactoryGen uint64
}

// ResolveProjectRoot walks up from startDir looking for a .git directory.
// Returns the directory containing .git, or startDir itself if none is found.
// The result is absolute when filepath.Abs succeeds, otherwise cleaned as-is.
func ResolveProjectRoot(startDir string) string {
	absDir, err := filepath.Abs(startDir)
	if err != nil {
		return filepath.Clean(startDir)
	}
	for dir := absDir; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	return absDir
}

// New creates a session in editor-only mode. Call SetAgent to enable the
// agent after construction (this breaks the circular dependency between
// session-as-workspace and agent-needing-workspace).
// The projectRoot is used for file listing and resolving relative paths.
//
// If e is nil, a fresh empty editor is installed so activeEditor is never
// nil — the same invariant cleanupDeletedPath enforces on file deletion.
// Callers that pass nil (e.g. tests for non-editor subsystems) get a sound
// session instead of one that panics on the first agent turn.
func New(e *editor.Editor, projectRoot string) *Session {
	if e == nil {
		e = editor.New(buffer.New())
	}
	// Normalize to absolute so CanonPath/resolvePath work regardless of
	// whether the caller passes ".", a relative path, or an absolute path.
	if absRoot, err := filepath.Abs(projectRoot); err == nil {
		projectRoot = filepath.Clean(absRoot)
	} else {
		projectRoot = filepath.Clean(projectRoot)
	}
	editors := make(map[string]*editor.Editor)
	contextSet := make(map[string]bool)
	s := &Session{
		activeEditor:  e,
		editors:       editors,
		contextSet:    contextSet,
		modifiedFiles: make(map[string]bool),
		projectRoot:   projectRoot,
		sink:          capture.NoopSink{},
		sessionID:     newSessionID(),
	}
	if e.Buf.Path != "" {
		if _, err := s.resolvePath(e.Buf.Path); err != nil {
			slog.Warn("New: initial editor path rejected", "path", e.Buf.Path, "err", err)
		} else {
			canon := s.CanonPath(e.Buf.Path)
			s.activeFile = canon
			s.editors[canon] = e
			if !s.isProjectMeta(canon) {
				s.contextSet[canon] = true
			}
		}
	}
	// Load persisted context — adds to whatever was set above.
	s.loadContext()
	// Persist so the initial file appears in .project/context.md.
	if len(s.contextSet) > 0 {
		s.saveContext()
	}
	return s
}

// SetEventSink installs the capture sink that receives session events
// (intent, proposal, accepted, rejected, validator). Pass the live
// adapter at the composition root; passing nil resets to the no-op sink
// so every internal call site can dispatch without nil checks.
//
// Safe to call once during wiring; not designed for hot-path swaps.
func (s *Session) SetEventSink(sink capture.SessionEventSink) {
	if sink == nil {
		sink = capture.NoopSink{}
	}
	s.sink = sink
}

// SessionID returns the identifier that groups this process's capture
// events. Adapters may use it as a per-session document path component.
func (s *Session) SessionID() string { return s.sessionID }

// validatorStagesPayload projects the frontend-facing ValidatorSummary
// slice into a JSON-serialisable form for capture payloads. Kept as a
// helper so the HandleEvent call site stays readable and future
// summary-field additions do not ripple through inline map literals.
func validatorStagesPayload(summaries []event.ValidatorSummary) []map[string]any {
	out := make([]map[string]any, 0, len(summaries))
	for _, s := range summaries {
		entry := map[string]any{
			"stage":   s.Stage,
			"verdict": s.Verdict,
		}
		if s.Feedback != "" {
			entry["feedback"] = s.Feedback
		}
		out = append(out, entry)
	}
	return out
}

// emitCapture fires a capture event through the configured sink. Errors
// are logged at warn level — capture failures must never block the
// session's hot path or surface to the developer.
func (s *Session) emitCapture(kind string, payload map[string]any) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ev := capture.Event{
		Kind:      kind,
		Timestamp: time.Now(),
		SessionID: s.sessionID,
		Payload:   payload,
	}
	if err := s.sink.Append(ctx, ev); err != nil {
		slog.Warn("session capture append failed", "kind", kind, "err", err)
	}
}

// SetHighlighterFactory installs a factory used to construct highlighters
// for every editor this session creates. Pass nil to disable highlighting.
// Typical usage: the TUI wires in [highlight.NewHighlighter] at startup;
// headless binaries never call this so no grammar blobs are linked in.
//
// All currently-open editors are re-decorated to match the new factory
// (their old highlighters are closed). Concurrent factory swaps and
// editor decoration are linearized via the highlighterFactoryGen counter:
// decorateEditor re-checks the generation after installing a highlighter
// and retries if a swap happened mid-install.
func (s *Session) SetHighlighterFactory(fn editor.HighlighterFactory) {
	s.mu.Lock()
	s.highlighterFactory = fn
	s.highlighterFactoryGen++
	// Snapshot under the lock; call SetHighlighter after releasing so the
	// editor's Close() path on the old highlighter doesn't run while we
	// hold the session lock.
	open := make([]*editor.Editor, 0, len(s.editors))
	for _, e := range s.editors {
		open = append(open, e)
	}
	s.mu.Unlock()

	for _, e := range open {
		s.decorateEditor(e)
	}
}

// newEditor constructs an editor and installs a highlighter when a factory
// has been configured. Session-internal editor construction goes through
// this helper so every editor the session manages is decorated uniformly.
//
// Caller must NOT hold s.mu — decorateEditor takes RLock. Use editor.New
// directly at sites that hold s.mu (write), then call decorateEditor after
// releasing it.
func (s *Session) newEditor(buf *buffer.Buffer) *editor.Editor {
	e := editor.New(buf)
	s.decorateEditor(e)
	return e
}

// decorateEditor installs a highlighter on an already-constructed editor
// to match the session's current factory. Safe to call on a nil or
// path-less editor — it no-ops. Must be called with s.mu unlocked.
//
// Linearizable w.r.t. concurrent [SetHighlighterFactory] via a
// generation counter: read (factory, gen) under RLock, install the
// highlighter outside the lock, then re-read gen — if it changed, the
// factory was swapped mid-install and we retry with the new one. Each
// retry overwrites the previous highlighter (SetHighlighter closes the
// old one), so no resources leak.
func (s *Session) decorateEditor(e *editor.Editor) {
	if e == nil || e.Buf == nil || e.Buf.Path == "" {
		return
	}
	for {
		s.mu.RLock()
		factory := s.highlighterFactory
		gen := s.highlighterFactoryGen
		s.mu.RUnlock()

		if factory == nil {
			e.SetHighlighter(nil)
		} else {
			e.SetHighlighter(factory(e.Buf.Path))
		}

		s.mu.RLock()
		stable := gen == s.highlighterFactoryGen
		s.mu.RUnlock()
		if stable {
			return
		}
	}
}

// SetAgent wires an agent into the session. The agent is typically created
// with the session as its Workspace, so this must be called after New.
// Safe to call more than once — later calls replace the bound agent (used
// when the first agent is constructed after startup, e.g. after OAuth).
func (s *Session) SetAgent(ag agentPort, events <-chan event.Event) {
	s.mu.Lock()
	s.agent = ag
	s.events = events
	s.mu.Unlock()
}

// SetDistributedMemory records which MCP servers are classified as distributed
// (team/shared) memory. The frontend reads this via DistributedMemory() for
// status display. Guarded by mu for safe cross-goroutine access.
func (s *Session) SetDistributedMemory(names []string) {
	s.mu.Lock()
	s.distributedMemory = names
	s.mu.Unlock()
}

// DistributedMemory returns the names of MCP servers classified as distributed memory.
func (s *Session) DistributedMemory() []string {
	s.mu.RLock()
	dm := s.distributedMemory
	s.mu.RUnlock()
	return dm
}

// SetLLMInfo stores the active LLM model ID and profile name.
// Called during startup and after model switches. Guarded by mu
// for safe cross-goroutine access.
func (s *Session) SetLLMInfo(modelID, profile string) {
	s.mu.Lock()
	s.llmModel = modelID
	s.llmProfile = profile
	s.mu.Unlock()
}

// LLMModel returns the full model ID (e.g. "google/gemini-2.5-flash").
func (s *Session) LLMModel() string {
	s.mu.RLock()
	m := s.llmModel
	s.mu.RUnlock()
	return m
}

// LLMProfile returns the active LLM profile name.
func (s *Session) LLMProfile() string {
	s.mu.RLock()
	p := s.llmProfile
	s.mu.RUnlock()
	return p
}

// SetModelSwitcher injects the model-switching function. The function should
// resolve the profile, wire auth (OAuth/stored keys), create a new provider,
// and hot-swap it on the agent. Called once during startup wiring.
func (s *Session) SetModelSwitcher(fn func(profile, modelID string) (string, error)) {
	s.mu.Lock()
	s.switchModel = fn
	s.mu.Unlock()
}

// SwitchModel switches the active LLM provider and model via the injected
// switcher callback. Returns the display model name. Persistence behavior
// (if any) is determined by the callback implementation.
func (s *Session) SwitchModel(profile, modelID string) (string, error) {
	s.mu.RLock()
	switcher := s.switchModel
	s.mu.RUnlock()
	if switcher == nil {
		return "", fmt.Errorf("model switching not available")
	}
	return switcher(profile, modelID)
}

// SetContext sets the application-level context. Agent runs derive a child
// context so they are cancelled when the application shuts down. Call this
// before starting any agent conversations.
func (s *Session) SetContext(ctx context.Context) {
	s.ctx = ctx
}

// SetEvents sets the event channel for frontends to read.
// Used when there is no agent but other event sources (e.g. LSP) need
// to reach the frontend.
func (s *Session) SetEvents(events <-chan event.Event) {
	s.mu.Lock()
	s.events = events
	s.mu.Unlock()
}

// Events returns the event channel for the frontend to read.
func (s *Session) Events() <-chan event.Event {
	s.mu.RLock()
	ch := s.events
	s.mu.RUnlock()
	return ch
}

// HasAgent returns true if the session has an active agent.
func (s *Session) HasAgent() bool {
	s.mu.RLock()
	has := s.agent != nil
	s.mu.RUnlock()
	return has
}

// Phase returns the current workflow phase (planning vs execution).
func (s *Session) Phase() Phase {
	return s.phase
}

// CurrentIntent returns the active goal string.
func (s *Session) CurrentIntent() string {
	return s.currentIntent
}

// IntentDone reports whether the current intent was completed.
func (s *Session) IntentDone() bool {
	return s.intentDone
}

// PendingEdit returns the edit currently awaiting approval, or nil if none.
func (s *Session) PendingEdit() *event.PendingEdit {
	return s.pendingEdit
}

// IntentHistory returns the resolved intents archived in order.
func (s *Session) IntentHistory() []string {
	return s.intentHistory
}

// LSP bridge methods (SetLanguageService, HasLanguageService,
// Diagnostics, LookupDefinition, PushNav/PopNav/GoBack, NavigateAgent,
// HoverInfo, RequestCompletion, RequestCompletionInContext,
// NotifySaved, wireBufferSync, unwireBufferSync) live in lsp_bridge.go.

// ActiveFile returns the path of the currently active file.
// Safe to call from any goroutine — reads under mu.RLock to
// avoid racing with SwitchTo.
func (s *Session) ActiveFile() string {
	s.mu.RLock()
	f := s.activeFile
	s.mu.RUnlock()
	return f
}

// ActiveEditor returns the currently active editor. Safe to call from
// any goroutine — reads under mu.RLock to avoid racing with SwitchTo.
func (s *Session) ActiveEditor() *editor.Editor {
	s.mu.RLock()
	e := s.activeEditor
	s.mu.RUnlock()
	return e
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

// SaveDirtyBuffers writes all modified (unsaved) buffers to disk and
// notifies the language service of each save. Returns the canonical
// paths of files that were successfully saved. Called by the agent
// before tool execution so it always sees the developer's latest edits.
func (s *Session) SaveDirtyBuffers() ([]string, error) {
	s.mu.RLock()
	// Snapshot dirty editors under read lock — Save() does I/O so we
	// don't want to hold the lock through os.WriteFile.
	type dirty struct {
		canon string
		ed    *editor.Editor
	}
	var toSave []dirty
	for path, e := range s.editors {
		if e.Buf.Modified {
			toSave = append(toSave, dirty{canon: path, ed: e})
		}
	}
	s.mu.RUnlock()

	var saved []string
	var firstErr error
	for _, d := range toSave {
		if err := d.ed.Save(); err != nil {
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

// EditorForPath returns the editor for a given path, or nil if not open.
func (s *Session) EditorForPath(path string) *editor.Editor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.editors[s.CanonPath(path)]
}

// --- Provenance ---

// FileStatus returns the provenance status of a file.
// inContext: the file is in the agent's context set (editable by agent).
// agentModified: the file was modified by an agent edit this session.
func (s *Session) FileStatus(path string) (inContext, agentModified bool) {
	canon := s.CanonPath(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contextSet[canon], s.modifiedFiles[canon]
}

// Search runs a project-wide text search from the project root.
func (s *Session) Search(pattern string, opts search.Options) ([]search.Result, error) {
	return search.Search(s.projectRoot, pattern, opts)
}

// AgentModifiedFiles returns paths of files modified by agent edits this
// session, as sorted relative paths.
func (s *Session) AgentModifiedFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files := make([]string, 0, len(s.modifiedFiles))
	for path := range s.modifiedFiles {
		rel, err := filepath.Rel(s.projectRoot, path)
		if err != nil {
			rel = path
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files
}

// Context-set methods (AddContext, RemoveContext, InContext,
// ContextFiles, loadContext, saveContext, contextPath, isProjectMeta)
// live in context.go.

// --- Workspace Implementation ---
// These methods satisfy the agent.Workspace interface, giving tools
// access to open buffers and the filesystem.

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
	e := s.newEditor(buf)
	canon := s.CanonPath(absPath)
	addToContext := !s.isProjectMeta(canon)
	s.mu.Lock()
	s.editors[canon] = e
	if addToContext {
		s.contextSet[canon] = true
	}
	s.mu.Unlock()

	// Wire LSP sync for the new editor.
	s.wireBufferSync(e)

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

// DeleteFile removes a file or directory from disk and cleans up session state.
// Closes editor buffers, removes context/modified entries, and unwires LSP sync
// for the deleted path and any children. If the active editor is affected, falls
// back to another open editor or installs a fresh empty one.
func (s *Session) DeleteFile(path string) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}

	canon := s.CanonPath(absPath)

	if canon == filepath.Clean(s.projectRoot) {
		return fmt.Errorf("cannot delete project root")
	}

	if canon == filepath.Join(filepath.Clean(s.projectRoot), ".project") || s.isProjectMeta(canon) {
		return fmt.Errorf("cannot delete project metadata: %s", path)
	}

	// Block deletion while an edit is pending approval or mid-animation,
	// same guard as SwitchTo. Approval state may reference the deleted path.
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		return ErrEditPending
	}

	if err := removeFromDisk(absPath, path); err != nil {
		return err
	}

	removed := s.cleanupDeletedPath(canon)
	for _, e := range removed {
		s.unwireBufferSync(e)
		e.Close()
	}

	s.saveContext()
	return nil
}

// removeFromDisk removes a file or directory from disk.
func removeFromDisk(absPath, displayPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("delete %s: %w", displayPath, err)
	}
	if info.IsDir() {
		err = os.RemoveAll(absPath)
	} else {
		err = os.Remove(absPath)
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", displayPath, err)
	}
	return nil
}

// cleanupDeletedPath removes all session state (editors, context, modified)
// for the given canonical path and any children (if a directory was deleted).
// Returns removed editors so the caller can unwire and close them.
func (s *Session) cleanupDeletedPath(canon string) []*editor.Editor {
	dirPrefix := canon + string(filepath.Separator)
	matches := func(p string) bool {
		return p == canon || strings.HasPrefix(p, dirPrefix)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []*editor.Editor
	for p, e := range s.editors {
		if matches(p) {
			removed = append(removed, e)
			delete(s.editors, p)
			if s.activeFile == p {
				s.activeFile = ""
				s.activeEditor = nil
			}
		}
	}
	deleteMatching(s.contextSet, matches)
	deleteMatching(s.modifiedFiles, matches)

	// Ensure s.activeEditor is never nil.
	if s.activeEditor == nil {
		for p, ed := range s.editors {
			s.activeEditor = ed
			s.activeFile = p
			break
		}
		if s.activeEditor == nil {
			e := editor.New(buffer.New())
			s.activeEditor = e
			// Track the empty editor so Session.Close() can release its resources.
			s.editors[""] = e
			s.activeFile = ""
		}
	}
	return removed
}

// deleteMatching removes all entries from a map whose keys satisfy pred.
func deleteMatching(m map[string]bool, pred func(string) bool) {
	for k := range m {
		if pred(k) {
			delete(m, k)
		}
	}
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

// SubmitGoal sends a message to the agent. If the agent is waiting for input
// (mid-conversation), the message continues the existing conversation.
// Otherwise, a new conversation is started. Returns true when the message
// continued an existing conversation, false when a new one was started.
//
// Must be called from a single goroutine (the TUI main goroutine).
func (s *Session) SubmitGoal(goal string) bool {
	if !s.HasAgent() {
		return false
	}

	s.emitCapture("intent", map[string]any{
		"goal":  goal,
		"phase": phaseName(s.phase),
	})

	// Handle planning phase commands.
	if s.phase == PhasePlanning {
		return s.handlePlanningInput(goal)
	}

	// Continue existing conversation or resume a saved one.
	// Reply handles both: queuing to a running agent, and resuming
	// from saved messages when the run has exited.
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if s.agent.Reply(ctx, goal) {
		return true
	}

	// No conversation to continue or resume — start fresh.
	s.startNewConversation(goal, event.ModeExecution)
	s.phase = PhaseExecution
	return false
}

// SubmitPlanningGoal starts a new conversation in planning mode.
// Write-side tools are disabled; the agent discusses design before coding.
// No-op if no agent is configured.
func (s *Session) SubmitPlanningGoal(goal string) {
	if !s.HasAgent() {
		return
	}

	s.startNewConversation(goal, event.ModePlanning)
	s.phase = PhasePlanning
}

// handlePlanningInput processes input during the planning phase.
// ":done" transitions to execution with the original goal.
// ":skip" transitions to execution immediately.
// Other input continues the planning conversation.
func (s *Session) handlePlanningInput(input string) bool {
	switch input {
	case ":done":
		// Transition to execution — cancel planning, start execution
		// with the original goal. The plan is persisted in demarkus
		// by the planning agent, so the execution agent picks it up
		// via memory summary.
		originalGoal := s.currentIntent
		s.agent.Cancel()
		// Reload work tree — the planning agent may have published
		// or updated /project.md during the conversation.
		if err := s.workTree.Reload(); err != nil {
			slog.Warn("session: reload work tree after planning", "err", err)
		}
		s.startNewConversation(originalGoal, event.ModeExecution)
		s.phase = PhaseExecution
		return false

	case ":skip":
		// Skip planning entirely — start execution with original goal.
		originalGoal := s.currentIntent
		s.agent.Cancel()
		s.startNewConversation(originalGoal, event.ModeExecution)
		s.phase = PhaseExecution
		return false

	default:
		// Continue planning conversation.
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		return s.agent.Reply(ctx, input)
	}
}

// startNewConversation archives any previous intent, clears stale state,
// and starts a new agent conversation in the specified mode.
func (s *Session) startNewConversation(goal string, mode event.Mode) {
	if s.currentIntent != "" && !s.intentDone {
		s.ArchiveIntent()
	}
	// Clear stale pending edit from previous run — Agent.Run cancels the
	// prior run internally, so any pending approval is no longer valid.
	s.pendingEdit = nil
	s.editReviewed = false
	s.currentIntent = goal
	s.intentDone = false
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.agent.RunWithMode(ctx, s.activeFile, s.activeEditor.Buf.Content(), goal, s.ContextFiles(), mode)
}

// ArchiveIntent marks the current intent as done.
// The intent stays visible as completed until the developer starts a new one.
func (s *Session) ArchiveIntent() {
	if s.currentIntent != "" {
		s.intentDone = true
		s.intentHistory = append(s.intentHistory, s.currentIntent)
	}
}

// ClearIntent cancels the current intent without archiving.
// Used when the developer explicitly escapes/deletes the intent.
func (s *Session) ClearIntent() {
	s.currentIntent = ""
	s.intentDone = false
}

// CancelAgent cancels the current agent run, clears intent, and
// resets every approval-flow field so a cancel mid-staged-apply does
// not leave stagedEditFile/pendingApproval latched — those gate
// SwitchTo/ReloadFile/DeleteFile, so leaking them locks the editor
// against further file operations until restart.
func (s *Session) CancelAgent() {
	if s.HasAgent() {
		s.ClearIntent()
		s.agent.Cancel()
		s.pendingEdit = nil
		s.pendingProposedReplace = ""
		s.pendingApproval = nil
		s.stagedEditFile = ""
		s.editReviewed = false
	}
}

// ErrEditPending is returned by SwitchTo when a file switch is blocked
// because an edit is pending approval or an animated edit is in progress.
var ErrEditPending = errors.New("cannot switch files while an edit is pending")

// SwitchTo switches the active editor to a different file. If the file is
// already open, switches to it. If not, opens it from disk. Does NOT cancel
// the agent or clear intent — multi-file work continues across switches.
// Returns ErrEditPending if an edit is awaiting approval or mid-animation.
// Returns an error if the file cannot be opened.
func (s *Session) SwitchTo(path string) error {
	canon := s.CanonPath(path)

	s.mu.Lock()

	// Block switching while an edit is pending approval.
	// PendingEdit covers the review phase; stagedEditFile covers the
	// post-PrepareApproval window before CompleteApproval/AbortApproval.
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		s.mu.Unlock()
		return ErrEditPending
	}

	// Already active
	if canon == s.activeFile {
		s.mu.Unlock()
		if !s.isProjectMeta(canon) && !s.InContext(canon) {
			s.AddContext(canon)
		}
		return nil
	}

	// Check if already open
	if e, ok := s.editors[canon]; ok {
		s.activeEditor = e
		s.activeFile = canon
		s.mu.Unlock()
		if !s.isProjectMeta(canon) && !s.InContext(canon) {
			s.AddContext(canon)
		}
		return nil
	}

	// Open from disk
	absPath, err := s.resolvePath(path)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("open %s: %w", path, err)
	}
	e := editor.New(buf)
	s.editors[canon] = e
	addToContext := !s.isProjectMeta(canon)
	if addToContext {
		s.contextSet[canon] = true
	}
	s.activeEditor = e
	s.activeFile = canon
	s.mu.Unlock()

	// Decorate after unlock — decorateEditor takes s.mu.RLock.
	s.decorateEditor(e)

	// Wire LSP sync for the new editor.
	s.wireBufferSync(e)

	if addToContext {
		s.saveContext()
	}
	return nil
}

// ReloadFile re-reads a file from disk, replacing the in-memory buffer content.
// If the file is not currently open in the editors map, this is a no-op.
// Returns ErrEditPending if an edit is being reviewed or animated.
func (s *Session) ReloadFile(path string) error {
	canon := s.CanonPath(path)

	s.mu.Lock()
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		s.mu.Unlock()
		return ErrEditPending
	}
	e, ok := s.editors[canon]
	s.mu.Unlock()

	if !ok {
		return nil // not open — nothing to reload
	}
	if err := e.Buf.ReloadFromDisk(); err != nil {
		return fmt.Errorf("reload %s: %w", path, err)
	}
	// Clamp cursor and scroll to safe positions after content change.
	e.MoveCursorTo(e.CursorLine, e.CursorCol)
	e.ClampScroll()
	slog.Debug("file reloaded from disk", "path", canon)
	return nil
}

// Close frees resources for all open editors.
func (s *Session) Close() {
	// Notify language service of all document closes, then shut it down.
	if s.langSyncer != nil {
		s.mu.Lock()
		for path := range s.wiredEditors {
			s.langSyncer.DidClose(path)
		}
		s.wiredEditors = nil
		s.mu.Unlock()
		if err := s.langSyncer.Close(); err != nil {
			slog.Warn("close language service", "err", err)
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.editors {
		e.Close()
	}
}

// Edit-approval methods (ReviewEdit, ApproveEdit, PrepareApproval,
// ApprovalPlan, CompleteApproval, AbortApproval, RejectEdit,
// computeLineOrigins) live in approval.go.

// --- Agent Event Handling ---

// HandleEvent processes an engine event and updates session state.
func (s *Session) HandleEvent(ev event.Event) {
	switch e := ev.(type) {
	case event.AgentEditProposed:
		s.pendingEdit = &e.Edit
		s.pendingProposedReplace = e.Edit.Replace
		s.editReviewed = false
		s.emitCapture("proposal", map[string]any{
			"id":      e.Edit.ID,
			"path":    e.Edit.Path,
			"search":  e.Edit.Search,
			"replace": e.Edit.Replace,
			"reason":  e.Edit.Reason,
		})
		if len(e.ValidatorSummaries) > 0 {
			s.emitCapture("validator", map[string]any{
				"proposal_id": e.Edit.ID,
				"path":        e.Edit.Path,
				"stages":      validatorStagesPayload(e.ValidatorSummaries),
			})
		}
	case event.AgentError:
		s.pendingEdit = nil
		s.pendingProposedReplace = ""
		s.pendingApproval = nil
		s.stagedEditFile = ""
		s.editReviewed = false
		_ = e // error text is in the event for the frontend to display
	case event.AgentDone:
		s.pendingEdit = nil
		s.pendingProposedReplace = ""
		s.pendingApproval = nil
		s.stagedEditFile = ""
		s.editReviewed = false
		if e.Success {
			s.ArchiveIntent()
		}
	case event.AgentFileCreated:
		// File is already opened by workspace.WriteFile — frontend can
		// render it in the project view or switch to it.
		_ = e
	case event.AgentWaiting:
		// Agent finished its turn, waiting for developer input.
		// No session state changes — intent stays active.
	case event.AgentStatus:
		// Status events are forwarded to the frontend's status chip;
		// session state does not mutate based on chip transitions.
	case event.AgentToken, event.AgentToolCall, event.AgentNavigate:
		// No session state changes — frontend renders these directly.
	case event.AgentTurnUsage, event.AgentInputEstimate:
		// Telemetry — no session state changes, frontend renders these.
	case event.FlushBuffers, event.ReloadBuffers:
		// Handled by the frontend directly — no session state to mutate.
	case event.DiagnosticsUpdated:
		// Frontend-only notification; no session state to mutate.
	default:
		slog.Debug("HandleEvent: unhandled event type", "type", fmt.Sprintf("%T", ev))
	}
}

// editorForEdit returns the editor targeted by the current pending edit.
// Falls back to the active editor if no path is set (backward compat).
// If the target file isn't open yet, auto-opens it from disk — the agent
// may have read the file via read_file (which doesn't create a buffer)
// and then proposed an edit_file on it.
//
// Callers must hold a non-nil s.pendingEdit — all public entry points
// (ReviewEdit, ApproveEdit, PrepareApproval) early-return before reaching
// here, so the nil case is not defended against.
func (s *Session) editorForEdit() *editor.Editor {
	path := s.pendingEdit.Path
	if path == "" {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.activeEditor
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
	e = s.newEditor(buf)
	s.mu.Lock()
	s.editors[canon] = e
	s.mu.Unlock()

	// Wire LSP sync for auto-opened editor.
	s.wireBufferSync(e)

	return e
}
