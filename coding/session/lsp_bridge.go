package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/engine/openfile"
)

// LSP-bridge methods on Session — wrapping the optional language
// service port. State (langSyncer, wiredEditors, navStack,
// completionMu) lives on Session because lifecycle and editor wiring
// interleave with SwitchTo / WriteFile / DeleteFile. The methods are
// grouped here so the LSP responsibility is visible at the file level
// rather than buried in session.go.
//
// Buffer-sync hooks (wireBufferSync, unwireBufferSync) live here as
// well — they straddle editor mutation and language-service signalling
// and belong to the same conceptual subsystem.

// SetLanguageService injects the language service port (e.g., lsp.Manager).
// Session depends on the lang.DocumentSyncer interface, not any concrete type.
// Wires Buffer.OnChange for every already-open file so changes auto-sync
// with LSP.
func (s *Session) SetLanguageService(syncer lang.DocumentSyncer) {
	s.langSyncer = syncer
	s.mu.RLock()
	files := make([]*openfile.OpenFile, 0, len(s.openFiles))
	for _, of := range s.openFiles {
		files = append(files, of)
	}
	s.mu.RUnlock()
	for _, of := range files {
		s.wireBufferSync(of)
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
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
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

// NavigateAgent handles an agent-initiated navigation: switches to the
// requested file if it is not already active. Cursor placement is the
// frontend's responsibility — Session no longer holds an editor
// controller, so the navigate event the agent emits carries (line, col)
// and the frontend applies them on its own editor instance after the
// switch lands.
//
// Returns an error from SwitchTo on failure.
func (s *Session) NavigateAgent(path string) error {
	if s.CanonPath(path) == s.ActiveFile() {
		return nil
	}
	return s.SwitchTo(path)
}

// GoBack pops the navigation stack and returns to the previous location.
// Returns the location jumped to, or nil if the stack is empty. The
// frontend is responsible for placing its cursor at the returned
// location — Session does not hold a UI cursor.
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
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
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

	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
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

	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
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

// wireBufferSync sets up Buffer.OnChange on an open file's buffer to
// auto-sync incremental changes with the language service. Also sends
// DidOpen. Safe to call multiple times — no-op if the file is already
// wired. Composes with any existing OnChange handler (does not
// overwrite). Uses canonical paths consistently to match the session's
// open-files map.
func (s *Session) wireBufferSync(of *openfile.OpenFile) {
	if s.langSyncer == nil || of == nil || of.Buf.Path == "" {
		return
	}
	canon := s.CanonPath(of.Buf.Path)
	languageID := lang.DetectLanguage(canon)
	if languageID == "" {
		return
	}

	// Check/update wiredFiles under lock — may be called from
	// TUI goroutine (SwitchTo) or agent goroutine (openFileForEdit, WriteFile).
	s.mu.Lock()
	if s.wiredFiles == nil {
		s.wiredFiles = make(map[string]func())
	}
	if _, alreadyWired := s.wiredFiles[canon]; alreadyWired {
		s.mu.Unlock()
		return
	}
	// Store the previous OnChange handler so unwireBufferSync can restore it.
	s.wiredFiles[canon] = of.Buf.OnChange
	s.mu.Unlock()

	// Open document in language service.
	s.langSyncer.DidOpen(canon, languageID, of.Buf.Content())

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
	// Concurrency: this read-modify-write on of.Buf.OnChange is safe because
	// wireBufferSync is only called on files that were just opened (no
	// other goroutine has a reference yet) or during startup before the TUI
	// and agent goroutines exist. The wiredFiles guard ensures at-most-once.
	buf := of.Buf // capture for closure
	prev := of.Buf.OnChange
	of.Buf.OnChange = func() {
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

// unwireBufferSync sends DidClose and removes the wired state for an
// open file. Used when a file is removed from the session (e.g., file
// close / delete).
func (s *Session) unwireBufferSync(of *openfile.OpenFile) {
	if s.langSyncer == nil || of == nil || of.Buf.Path == "" {
		return
	}
	canon := s.CanonPath(of.Buf.Path)

	s.mu.Lock()
	prev, wired := s.wiredFiles[canon]
	if wired {
		delete(s.wiredFiles, canon)
	}
	s.mu.Unlock()

	if wired {
		// Restore the previous OnChange handler, removing the LSP closure.
		// Prevents stale DidChange calls if the old buffer is mutated after close.
		of.Buf.OnChange = prev
		s.langSyncer.DidClose(canon)
	}
}
