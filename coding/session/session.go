// Package session orchestrates the developer-agent collaboration workflow.
// It owns the intent lifecycle, pending edit state, and the approve/reject
// flow — domain logic that every frontend must enforce identically.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/latebit-io/nib/engine/search"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/capture"
	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/engine/openfile"
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
	// activeOpenFile points to the currently-focused open file. Updated
	// on file switch. Callers outside this package read via
	// ActiveOpenFile(); direct field access is reserved for session
	// internals so the workflow-enforcing methods remain the only
	// mutation path. UI state (cursor, selection, scroll, highlighter)
	// lives on the frontend's editor controller — Session never owns it.
	activeOpenFile *openfile.OpenFile

	// ctx is the application-level context. Agent runs derive a child
	// context so they are cancelled when the application shuts down.
	ctx context.Context

	agent  agentPort
	events <-chan event.Event // frontend reads engine events from here

	// mu guards openFiles and activeFile for concurrent access from the
	// TUI goroutine and agent goroutine (via Workspace interface).
	mu sync.RWMutex

	// Multi-buffer state
	openFiles   map[string]*openfile.OpenFile // path → open file handle
	activeFile  string                        // path of the active open file
	projectRoot string                        // root for file listing and path resolution

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

	// sessionID groups capture events emitted during this host process
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

	// wiredFiles tracks which open files have had LSP sync wired (by canonical path).
	// Maps to the previous OnChange handler that was installed before wiring,
	// so unwireBufferSync can restore it. nil func() means no previous handler.
	// Guarded by mu.
	wiredFiles map[string]func()

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

// New creates a session bound to an initial open file. Call SetAgent to
// enable the agent after construction (this breaks the circular dependency
// between session-as-workspace and agent-needing-workspace). The
// projectRoot is used for file listing and resolving relative paths.
//
// If of is nil, a fresh empty open file is installed so activeOpenFile is
// never nil — the same invariant cleanupDeletedPath enforces on file
// deletion. Callers that pass nil (e.g. tests for non-editor subsystems)
// get a sound session instead of one that panics on the first agent turn.
func New(of *openfile.OpenFile, projectRoot string) *Session {
	if of == nil {
		of = openfile.New(buffer.New())
	}
	// Normalize to absolute so CanonPath/resolvePath work regardless of
	// whether the caller passes ".", a relative path, or an absolute path.
	if absRoot, err := filepath.Abs(projectRoot); err == nil {
		projectRoot = filepath.Clean(absRoot)
	} else {
		projectRoot = filepath.Clean(projectRoot)
	}
	openFiles := make(map[string]*openfile.OpenFile)
	contextSet := make(map[string]bool)
	s := &Session{
		activeOpenFile: of,
		openFiles:      openFiles,
		contextSet:     contextSet,
		modifiedFiles:  make(map[string]bool),
		projectRoot:    projectRoot,
		sink:           capture.NoopSink{},
		sessionID:      newSessionID(),
	}
	if of.Buf.Path != "" {
		if _, err := s.resolvePath(of.Buf.Path); err != nil {
			slog.Warn("New: initial open file path rejected", "path", of.Buf.Path, "err", err)
		} else {
			canon := s.CanonPath(of.Buf.Path)
			s.activeFile = canon
			s.openFiles[canon] = of
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

// Capture-sink wiring (SetEventSink, SessionID, validatorStagesPayload,
// emitCapture) lives in capture.go.

// LLM, model selection, and highlighter wiring (SetHighlighterFactory,
// SetLLMInfo, LLMModel, LLMProfile, SetModelSwitcher, SwitchModel) live in
// llm.go.

// Editor lifecycle methods (newEditor, decorateEditor, ActiveEditor,
// ActiveFile, OpenFiles, EditorForPath, SwitchTo, ReloadFile, DeleteFile,
// cleanupDeletedPath, editorForEdit) and ErrEditPending live in editors.go.

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

// PendingEdit returns the edit currently awaiting approval, or nil if none.
func (s *Session) PendingEdit() *event.PendingEdit {
	return s.pendingEdit
}

// Intent lifecycle methods (CurrentIntent, IntentDone, IntentHistory,
// SubmitGoal, SubmitPlanningGoal, handlePlanningInput, startNewConversation,
// ArchiveIntent, ClearIntent, CancelAgent) live in intent.go.

// LSP bridge methods (SetLanguageService, HasLanguageService,
// Diagnostics, LookupDefinition, PushNav/PopNav/GoBack, NavigateAgent,
// HoverInfo, RequestCompletion, RequestCompletionInContext,
// NotifySaved, wireBufferSync, unwireBufferSync) live in lsp_bridge.go.

// ProjectRoot returns the project root path.
func (s *Session) ProjectRoot() string {
	return s.projectRoot
}

// ModifiedFiles returns paths of open files with unsaved changes.
func (s *Session) ModifiedFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var modified []string
	for path, of := range s.openFiles {
		if of.Modified() {
			modified = append(modified, path)
		}
	}
	return modified
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

// Workspace methods (ReadFile, WriteFile, ListFiles, ListFilesAndDirs,
// CanonPath, resolvePath, SaveDirtyBuffers, CreateDir) live in workspace.go.

// Close releases resources held by Session — chiefly the language service.
// Open file handles wrap a [*buffer.Buffer] which has no resources to close
// (the highlighter, which previously needed a Close, lives on the
// frontend's editor controller now).
func (s *Session) Close() {
	if s.langSyncer == nil {
		return
	}
	s.mu.Lock()
	for path := range s.wiredFiles {
		s.langSyncer.DidClose(path)
	}
	s.wiredFiles = nil
	s.mu.Unlock()
	if err := s.langSyncer.Close(); err != nil {
		slog.Warn("close language service", "err", err)
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
	case event.ReloadBuffers:
		// Handled by the frontend directly — no session state to mutate.
	case event.DiagnosticsUpdated:
		// Frontend-only notification; no session state to mutate.
	default:
		slog.Debug("HandleEvent: unhandled event type", "type", fmt.Sprintf("%T", ev))
	}
}
