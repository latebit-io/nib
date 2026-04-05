// Package session orchestrates the developer-agent collaboration workflow.
// It owns the intent lifecycle, pending edit state, and the approve/reject/continue
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

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/filelist"
	"github.com/latebit-io/junto/engine/lang"
	"github.com/latebit-io/junto/engine/project"
)

// agentPort is the narrow interface Session needs from an agent implementation.
// Defined here (not in the agent package) so Session depends on an abstraction,
// not a concrete type (DIP).
type agentPort interface {
	RunWithMode(ctx context.Context, fileName, fileContent, goal string, contextFiles []string, mode agent.Mode)
	Reply(input string) bool
	Cancel()
	Approve()
	Reject()
	Continue(path, bufferContent string)
}

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

	// lastEditedFile tracks which file was successfully edited. Used by
	// Continue to send the correct file's content even if the user switches
	// to a different file before pressing Continue.
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

	// Work tree — structured project hierarchy loaded from demarkus memory.
	// See work.go for loading, querying, and persistence.
	memoryStore    memoryStore   // optional demarkus store (nil when not configured)
	workTree       *project.Tree // parsed work hierarchy (nil when not loaded)
	workTreeVer    int           // demarkus version for optimistic concurrency
	workTreeDirty  bool          // true when in-memory changes need persisting
	workTreeModGen uint64        // incremented on each mutation; detects concurrent changes during save
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
func New(e *editor.Editor, projectRoot string) *Session {
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
		Editor:        e,
		editors:       editors,
		contextSet:    contextSet,
		modifiedFiles: make(map[string]bool),
		projectRoot:   projectRoot,
	}
	if e != nil && e.Buf.Path != "" {
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

// SetAgent wires an agent into the session. The agent is typically created
// with the session as its Workspace, so this must be called after New.
func (s *Session) SetAgent(ag agentPort, events <-chan event.Event) {
	s.agent = ag
	s.events = events
}

// SetEvents sets the event channel for frontends to read.
// Used when there is no agent but other event sources (e.g. LSP) need
// to reach the frontend.
func (s *Session) SetEvents(events <-chan event.Event) {
	s.events = events
}

// Events returns the event channel for the frontend to read.
func (s *Session) Events() <-chan event.Event {
	return s.events
}

// HasAgent returns true if the session has an active agent.
func (s *Session) HasAgent() bool {
	return s.agent != nil
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

// SetLanguageService injects the language service port (e.g., lsp.Manager).
// Session depends on the lang.DocumentSyncer interface, not any concrete type.
// Wires Buffer.OnChange for the current editor to auto-sync with LSP.
func (s *Session) SetLanguageService(syncer lang.DocumentSyncer) {
	s.langSyncer = syncer
	// Wire all already-open editors, not just the active one.
	s.mu.RLock()
	editors := make([]*editor.Editor, 0, len(s.editors))
	for _, e := range s.editors {
		editors = append(editors, e)
	}
	s.mu.RUnlock()
	for _, e := range editors {
		s.wireBufferSync(e)
	}
}

// HasLanguageService reports whether a language service is available.
func (s *Session) HasLanguageService() bool {
	return s.langSyncer != nil
}

// Diagnostics returns the current diagnostics for the given file path.
// Returns nil if no language service is available or it doesn't support diagnostics.
// This encapsulates the DiagnosticProvider capability check so frontends
// don't need to perform type assertions on the language service.
func (s *Session) Diagnostics(path string) []lang.Diagnostic {
	dp, ok := s.langSyncer.(lang.DiagnosticProvider)
	if !ok {
		return nil
	}
	return dp.Diagnostics(path)
}

// LookupDefinition queries the language service for the definition location
// without modifying session state. Safe to call from a background goroutine.
// Use GoToDefinition for the full navigation flow (nav stack + file switch).
func (s *Session) LookupDefinition(line, col int) (*lang.Location, error) {
	dp, ok := s.langSyncer.(lang.DefinitionProvider)
	if !ok {
		return nil, errors.New("language service does not support go-to-definition")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := s.ActiveFile()
	if path == "" {
		return nil, errors.New("no active file")
	}

	loc, err := dp.Definition(ctx, path, line, col)
	if err != nil {
		return nil, err
	}
	return &loc, nil
}

// PushNav records a position on the navigation stack for go-back.
// Accepts primitives so callers don't need to construct lang.Location.
func (s *Session) PushNav(path string, line, col int) {
	s.navStack = append(s.navStack, lang.Location{Path: path, Line: line, Col: col})
}

// PopNav removes the top entry from the navigation stack.
// No-op if the stack is empty. Used to undo a PushNav on navigation failure.
func (s *Session) PopNav() {
	if len(s.navStack) > 0 {
		s.navStack = s.navStack[:len(s.navStack)-1]
	}
}

// GoBack pops the navigation stack and returns to the previous location.
// Returns the location jumped to, or nil if the stack is empty.
func (s *Session) GoBack() *lang.Location {
	if len(s.navStack) == 0 {
		return nil
	}
	loc := s.navStack[len(s.navStack)-1]

	if loc.Path != s.ActiveFile() {
		if err := s.SwitchTo(loc.Path); err != nil {
			slog.Warn("GoBack: cannot switch file", "path", loc.Path, "err", err)
			return nil
		}
	}
	s.Editor.MoveCursorTo(loc.Line, loc.Col)
	s.navStack = s.navStack[:len(s.navStack)-1]
	return &loc
}

// HoverInfo returns type/documentation information for the symbol at the given position.
// Returns empty string if the capability is unavailable or no hover info exists.
func (s *Session) HoverInfo(line, col int) (string, error) {
	hp, ok := s.langSyncer.(lang.HoverProvider)
	if !ok {
		return "", errors.New("language service does not support hover")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := s.ActiveFile()
	if path == "" {
		return "", errors.New("no active file")
	}

	return hp.Hover(ctx, path, line, col)
}

// RequestCompletion queries the language service for completions at the given position.
// Safe to call from a background goroutine. Returns nil result if the
// capability is unavailable.
func (s *Session) RequestCompletion(path string, line, col int) (*lang.CompletionResult, error) {
	cp, ok := s.langSyncer.(lang.CompletionProvider)
	if !ok {
		return nil, errors.New("language service does not support completion")
	}
	if path == "" {
		return nil, errors.New("no active file")
	}
	path = s.CanonPath(path)

	s.completionMu.Lock()
	defer s.completionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return cp.Complete(ctx, path, line, col)
}

// RequestCompletionInContext queries completions against temporary file content.
// Atomically syncs tempContent to the LSP, requests completion, then reverts
// to originalContent. Safe to call from a background goroutine — all content
// snapshots must be captured by the caller on the TUI goroutine before dispatch.
// Used for completion inside diff overlays where the LSP hasn't seen the proposed code.
func (s *Session) RequestCompletionInContext(path, tempContent, originalContent string, line, col int) (*lang.CompletionResult, error) {
	cp, ok := s.langSyncer.(lang.CompletionProvider)
	if !ok {
		return nil, errors.New("language service does not support completion")
	}
	if path == "" {
		return nil, errors.New("no active file")
	}
	path = s.CanonPath(path)

	// Serialize overlay completions to prevent DidChange interleaving.
	s.completionMu.Lock()
	defer s.completionMu.Unlock()

	// Sync temporary content so LSP sees the overlay code.
	s.langSyncer.DidChange(path, []lang.TextChange{{Text: tempContent, FullContent: true}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := cp.Complete(ctx, path, line, col)

	// Always revert to original content, even on error.
	s.langSyncer.DidChange(path, []lang.TextChange{{Text: originalContent, FullContent: true}})

	return result, err
}

// NotifySaved notifies the language service that the current file was saved.
// Called by the frontend after a successful buffer save.
func (s *Session) NotifySaved() {
	if s.langSyncer == nil {
		return
	}
	path := s.ActiveFile()
	if path == "" {
		return
	}
	s.langSyncer.DidSave(s.CanonPath(path))
}

// wireBufferSync sets up Buffer.OnChange for an editor to auto-sync
// incremental changes with the language service. Also sends DidOpen.
// Safe to call multiple times — no-op if the editor is already wired.
// Composes with any existing OnChange handler (does not overwrite).
// Uses canonical paths consistently to match the session's editor map.
func (s *Session) wireBufferSync(e *editor.Editor) {
	if s.langSyncer == nil || e == nil || e.Buf.Path == "" {
		return
	}
	canon := s.CanonPath(e.Buf.Path)
	languageID := lang.DetectLanguage(canon)
	if languageID == "" {
		return
	}

	// Check/update wiredEditors under lock — may be called from
	// TUI goroutine (SwitchTo) or agent goroutine (editorForEdit, WriteFile).
	s.mu.Lock()
	if s.wiredEditors == nil {
		s.wiredEditors = make(map[string]func())
	}
	if _, alreadyWired := s.wiredEditors[canon]; alreadyWired {
		s.mu.Unlock()
		return
	}
	// Store the previous OnChange handler so unwireBufferSync can restore it.
	s.wiredEditors[canon] = e.Buf.OnChange
	s.mu.Unlock()

	// Open document in language service.
	s.langSyncer.DidOpen(canon, languageID, e.Buf.Content())

	// Check if the backend needs full document content instead of incremental
	// changes (e.g., UTF-32 encoding not negotiated, so rune-based positions
	// would be incorrect for non-BMP characters).
	fullSync := false
	if fcs, ok := s.langSyncer.(lang.FullContentSyncer); ok {
		fullSync = fcs.NeedsFullContentSync()
	}

	// Compose with existing OnChange handler (if any) so we don't
	// silently disconnect other observers. The previous handler runs first.
	// Note: DrainChanges returns and clears — if a future observer also
	// needs changes, Buffer should switch to a multi-subscriber model.
	//
	// Concurrency: this read-modify-write on e.Buf.OnChange is safe because
	// wireBufferSync is only called on editors that were just created (no
	// other goroutine has a reference yet) or during startup before the TUI
	// and agent goroutines exist. The wiredEditors guard ensures at-most-once.
	buf := e.Buf // capture for closure
	prev := e.Buf.OnChange
	e.Buf.OnChange = func() {
		if prev != nil {
			prev()
		}
		// Drain changes even in full-sync mode to prevent unbounded growth.
		changes := buf.DrainChanges()
		if len(changes) == 0 {
			return
		}
		if fullSync {
			// Full-content fallback: send entire buffer instead of
			// incremental changes with potentially incorrect positions.
			s.langSyncer.DidChange(canon, []lang.TextChange{{
				FullContent: true,
				Text:        buf.Content(),
			}})
			return
		}
		textChanges := make([]lang.TextChange, len(changes))
		for i, c := range changes {
			textChanges[i] = lang.TextChange{
				StartLine:   c.StartLine,
				StartCol:    c.StartCol,
				EndLine:     c.EndLine,
				EndCol:      c.EndCol,
				Text:        c.Text,
				FullContent: c.FullContent,
			}
		}
		s.langSyncer.DidChange(canon, textChanges)
	}
}

// unwireBufferSync sends DidClose and removes the wired state for an editor.
// Used when an editor is removed from the session (e.g., SwitchEditor).
func (s *Session) unwireBufferSync(e *editor.Editor) {
	if s.langSyncer == nil || e == nil || e.Buf.Path == "" {
		return
	}
	canon := s.CanonPath(e.Buf.Path)

	s.mu.Lock()
	prev, wired := s.wiredEditors[canon]
	if wired {
		delete(s.wiredEditors, canon)
	}
	s.mu.Unlock()

	if wired {
		// Restore the previous OnChange handler, removing the LSP closure.
		// Prevents stale DidChange calls if the old editor is mutated after close.
		e.Buf.OnChange = prev
		s.langSyncer.DidClose(canon)
	}
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

// AgentModifiedFiles returns paths of files modified by agent edits this
// Search runs a project-wide text search from the project root.
func (s *Session) Search(pattern string, opts search.Options) ([]search.Result, error) {
	return search.Search(s.projectRoot, pattern, opts)
}

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

// --- Context Set ---
// The context set defines which files the agent is allowed to edit.
// Files are auto-added when the developer opens them or when the agent
// creates new files. The developer can also add/remove files explicitly.

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
	for _, line := range strings.Split(string(data), "\n") {
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
		paths = append(paths, s.CanonPath(rel))
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

// --- Workspace Implementation ---
// These methods satisfy the agent.Workspace interface, giving tools
// access to open buffers and the filesystem.

// ReadFile returns a file's content from disk. Called from the agent goroutine
// via Workspace — reads from disk only to avoid data races with TUI-side
// buffer mutations. The agent's FileCache is the source of truth for
// in-flight content (seeded by Run, updated by Continue). This method is
// only called for files not yet in the cache.
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
	e := editor.New(buf)
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

// DeleteFile removes a file from disk and cleans up session state.
// Removes the file from the context set, closes its editor buffer,
// and unwires LSP sync. If the deleted file was the active editor,
// activeFile is cleared so the caller can switch to another buffer.
// CreateDir creates a directory (and parents) within the project root.
// The path is validated via resolvePath to prevent traversal outside the root.
func (s *Session) CreateDir(path string) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(absPath, 0o755)
}

func (s *Session) DeleteFile(path string) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}

	canon := s.CanonPath(absPath)

	if s.isProjectMeta(canon) {
		return fmt.Errorf("cannot delete project metadata: %s", path)
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
				s.Editor = nil
			}
		}
	}
	deleteMatching(s.contextSet, matches)
	deleteMatching(s.modifiedFiles, matches)

	// Ensure s.Editor is never nil.
	if s.Editor == nil {
		for p, ed := range s.editors {
			s.Editor = ed
			s.activeFile = p
			break
		}
		if s.Editor == nil {
			s.Editor = editor.New(buffer.New())
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

	// Handle planning phase commands.
	if s.phase == PhasePlanning {
		return s.handlePlanningInput(goal)
	}

	// Continue existing conversation if the agent is waiting for input.
	// Reply returns false if the agent raced out of the waiting state
	// or the channel is full — fall through to start a new conversation.
	if s.agent.Reply(goal) {
		return true
	}

	// Start a new conversation. Archive any previous intent.
	s.startNewConversation(goal, agent.ModeExecution)
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

	s.startNewConversation(goal, agent.ModePlanning)
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
		if err := s.loadWorkTree(); err != nil {
			slog.Warn("session: reload work tree after planning", "err", err)
		}
		s.startNewConversation(originalGoal, agent.ModeExecution)
		s.phase = PhaseExecution
		return false

	case ":skip":
		// Skip planning entirely — start execution with original goal.
		originalGoal := s.currentIntent
		s.agent.Cancel()
		s.startNewConversation(originalGoal, agent.ModeExecution)
		s.phase = PhaseExecution
		return false

	default:
		// Continue planning conversation.
		return s.agent.Reply(input)
	}
}

// startNewConversation archives any previous intent, clears stale state,
// and starts a new agent conversation in the specified mode.
func (s *Session) startNewConversation(goal string, mode agent.Mode) {
	if s.currentIntent != "" && !s.intentDone {
		s.ArchiveIntent()
	}
	// Clear stale pending edit from previous run — Agent.Run cancels the
	// prior run internally, so any pending approval is no longer valid.
	s.pendingEdit = nil
	s.editReviewed = false
	s.currentIntent = goal
	s.intentDone = false
	s.agent.RunWithMode(context.Background(), s.activeFile, s.Editor.Buf.Content(), goal, s.ContextFiles(), mode)
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

// CancelAgent cancels the current agent run, clears intent, and resets pending edit.
func (s *Session) CancelAgent() {
	if s.HasAgent() {
		s.ClearIntent()
		s.agent.Cancel()
		s.pendingEdit = nil
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

	// Block switching while an edit is pending approval or mid-animation.
	// PendingEdit covers the review phase; stagedEditFile covers the
	// animation phase (PrepareApproval clears PendingEdit but sets
	// stagedEditFile until CompleteApproval/AbortApproval).
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
		s.Editor = e
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
	s.Editor = e
	s.activeFile = canon
	s.mu.Unlock()

	// Wire LSP sync for the new editor.
	s.wireBufferSync(e)

	if addToContext {
		s.saveContext()
	}
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
	s.pendingEdit = nil
	s.editReviewed = false
	s.currentIntent = ""
	s.intentDone = false

	old := s.Editor

	// Notify LSP that the old document is closing.
	s.unwireBufferSync(old)

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

	// Wire LSP sync for the new editor.
	s.wireBufferSync(newEditor)

	return old
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
	if s.pendingEdit == nil {
		return nil, false
	}
	// editorForEdit may auto-open a file that isn't in the map yet.
	e := s.editorForEdit()
	if e == nil {
		return nil, false
	}
	// Auto-switch to the target file so the frontend shows the right buffer.
	// Done after editorForEdit so auto-opened files are also switched to.
	if s.pendingEdit.Path != "" {
		canon := s.CanonPath(s.pendingEdit.Path)
		s.mu.Lock()
		if canon != s.activeFile {
			s.Editor = e
			s.activeFile = canon
			switched = true
		}
		s.mu.Unlock()
	}
	diff = e.ComputeDiff(s.pendingEdit.Search, s.pendingEdit.Replace)
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
	if s.pendingEdit == nil || !s.HasAgent() {
		return false, "no pending edit"
	}
	if !s.editReviewed {
		return false, "edit not reviewed — call ReviewEdit first"
	}
	e := s.editorForEdit()
	if e == nil {
		s.agent.Reject()
		s.pendingEdit = nil
		s.editReviewed = false
		return false, "file not open"
	}
	editPath := s.activeFile
	if s.pendingEdit.Path != "" {
		editPath = s.CanonPath(s.pendingEdit.Path)
	}
	lineOrigins := computeLineOrigins(search, s.pendingEdit.Replace, replace)
	ok, reason := e.ApplyEdit(search, replace, lineOrigins)
	if ok {
		s.lastEditedFile = editPath
		s.mu.Lock()
		s.modifiedFiles[editPath] = true
		s.mu.Unlock()
		s.agent.Approve()
	} else {
		s.agent.Reject()
	}
	s.pendingEdit = nil
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
	// Line is the buffer line where the edit starts (0-indexed).
	Line int
	// Col is the buffer column where the edit starts (0-indexed, rune).
	Col int
	// Search is the text to delete from the buffer.
	Search string
	// Replace is the text to type into the buffer.
	Replace string
	// LineOrigins holds the provenance for each line of the replacement.
	// Index 0 corresponds to the buffer line at Line, index 1 to Line+1, etc.
	// A nil entry means "don't change this line's origin" (the line was
	// unchanged from the search text — the agent just re-included it as context).
	LineOrigins []*buffer.Origin
	// Narrowed contains the change region with unchanged prefix/suffix lines
	// excluded. When IsSurgical() is true, the frontend should use these
	// narrowed values instead of the full Search/Replace for animation —
	// unchanged lines stay in place and are never deleted from the buffer.
	Narrowed *editor.NarrowedEdit
}

// IsSurgical returns true if the edit has unchanged prefix or suffix lines
// that can be preserved during animation.
func (p *AnimationPlan) IsSurgical() bool {
	return p.Narrowed != nil && (p.Narrowed.PrefixLines > 0 || p.Narrowed.SuffixLines > 0)
}

// PrepareApproval validates the reviewed edit and returns an AnimationPlan.
// The frontend provides the final search/replace (possibly modified in the overlay).
// Clears pending edit state but does NOT mutate the buffer or signal the agent.
//
// Returns an error if there is no pending edit, the edit was not reviewed,
// or the search text cannot be uniquely located in the buffer.
func (s *Session) PrepareApproval(search, replace string) (*AnimationPlan, error) {
	if s.pendingEdit == nil || !s.HasAgent() {
		return nil, errors.New("no pending edit")
	}
	if !s.editReviewed {
		return nil, errors.New("edit not reviewed — call ReviewEdit first")
	}
	e := s.editorForEdit()
	if e == nil {
		s.agent.Reject()
		s.pendingEdit = nil
		s.editReviewed = false
		return nil, errors.New("file not open")
	}
	loc, reason := e.LocateEdit(search)
	if loc == nil {
		s.agent.Reject()
		s.pendingEdit = nil
		s.editReviewed = false
		return nil, errors.New(reason)
	}
	if s.pendingEdit.Path != "" {
		s.stagedEditFile = s.CanonPath(s.pendingEdit.Path)
	} else {
		s.stagedEditFile = s.activeFile
	}
	lineOrigins := computeLineOrigins(search, s.pendingEdit.Replace, replace)

	// Compute hunks and narrowed edit for surgical animation.
	searchLines := strings.Split(search, "\n")
	replaceLines := strings.Split(replace, "\n")
	hunks := editor.ComputeHunks(searchLines, replaceLines)

	var narrowed *editor.NarrowedEdit
	if editor.IsSurgical(hunks) {
		ne := editor.NarrowEdit(loc.Line, loc.Col, search, replace, hunks, lineOrigins)
		narrowed = &ne
	}

	s.pendingEdit = nil
	s.editReviewed = false
	return &AnimationPlan{
		Line:        loc.Line,
		Col:         loc.Col,
		Search:      search,
		Replace:     replace,
		LineOrigins: lineOrigins,
		Narrowed:    narrowed,
	}, nil
}

// computeLineOrigins determines per-line provenance for a replacement by
// comparing three versions: the original search text, the agent's proposed
// replacement, and the developer's final replacement (possibly modified in
// the overlay). Returns a slice with one entry per line of finalReplace:
//   - nil: line is identical in search and finalReplace — unchanged, keep current origin
//   - OriginAgent: agent changed this line and developer didn't modify it
//   - OriginDeveloper: developer modified this line in the overlay (or added it)
func computeLineOrigins(search, originalReplace, finalReplace string) []*buffer.Origin {
	searchLines := strings.Split(search, "\n")
	origLines := strings.Split(originalReplace, "\n")
	finalLines := strings.Split(finalReplace, "\n")

	origins := make([]*buffer.Origin, len(finalLines))
	for i := range finalLines {
		// Line unchanged from search — agent re-included it as context, skip
		if i < len(searchLines) && searchLines[i] == finalLines[i] {
			continue
		}
		// Line was changed; determine who changed it
		origin := buffer.OriginAgent
		if i >= len(origLines) || origLines[i] != finalLines[i] {
			origin = buffer.OriginDeveloper
		}
		origins[i] = &origin
	}
	return origins
}

// CompleteApproval signals the agent that the animated edit has been applied.
// Call this after the animation finishes (or after a yield). Promotes the
// staged edit path to lastEditedFile so Continue sends the right content.
func (s *Session) CompleteApproval() {
	if s.HasAgent() {
		s.lastEditedFile = s.stagedEditFile
		if s.stagedEditFile != "" {
			s.mu.Lock()
			s.modifiedFiles[s.stagedEditFile] = true
			s.mu.Unlock()
		}
		s.stagedEditFile = ""
		s.agent.Approve()
	}
}

// AbortApproval rejects a prepared approval that was never completed.
// Use this when the user cancels an in-progress animation. The agent
// receives a rejection and can try a different approach — unlike
// CancelAgent which kills the entire run.
func (s *Session) AbortApproval() {
	if s.HasAgent() {
		s.stagedEditFile = ""
		s.agent.Reject()
	}
}

// RejectEdit rejects the pending edit and signals the agent.
func (s *Session) RejectEdit() {
	if s.pendingEdit == nil || !s.HasAgent() {
		return
	}
	s.pendingEdit = nil
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

// HandleEvent processes an engine event and updates session state.
func (s *Session) HandleEvent(ev event.Event) {
	switch e := ev.(type) {
	case event.AgentEditProposed:
		s.pendingEdit = &e.Edit
		s.editReviewed = false
	case event.AgentError:
		s.pendingEdit = nil
		s.editReviewed = false
		_ = e // error text is in the event for the frontend to display
	case event.AgentDone:
		s.pendingEdit = nil
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
	case event.AgentToken, event.AgentStatus, event.AgentToolCall, event.AgentNavigate:
		// No session state changes — frontend renders these directly
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
func (s *Session) editorForEdit() *editor.Editor {
	if s.pendingEdit == nil {
		return s.Editor
	}
	path := s.pendingEdit.Path
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

	// Wire LSP sync for auto-opened editor.
	s.wireBufferSync(e)

	return e
}
