package ui

import (
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/engine/lang"
)

// LSP-backed async flows: go-to-definition, go-back, hover, autocomplete.
// Extracted from app.go (Phase 5 of AppModel decomposition).
//
// Pattern: each user action dispatches a tea.Cmd that runs the LSP request
// off the TUI goroutine, then a result message lands back on the TUI
// goroutine for state mutation. Stale-response detection compares the
// result's origin (path + cursor at request time) against the editor's
// current cursor; mismatches drop silently.
//
// Cursor sourcing: when an approval overlay is active the editor has a
// pseudo-cursor in the overlay's coordinate space; the LSP needs buffer
// coordinates. Helpers that touch cursor state map overlay → buffer via
// `Overlay.StartLine + overlayCursorLine`.

// goToDefResultMsg delivers go-to-definition results from an async LSP request.
type goToDefResultMsg struct {
	// Result from LSP — target location.
	path      string
	line, col int
	err       error
	// Origin cursor for nav stack push.
	originPath            string
	originLine, originCol int
}

// completionResultMsg delivers completion results from an async LSP request.
type completionResultMsg struct {
	items        []lang.CompletionItem
	isIncomplete bool
	path         string
	line, col    int
}

// completionTriggerMsg is emitted by the editor after typing a trigger character.
type completionTriggerMsg struct{}

// completionTickMsg fires after a debounce delay to trigger a completion request.
type completionTickMsg struct {
	path      string
	line, col int
}

// hoverResultMsg delivers hover information from an async LSP request.
// Carries the origin path+cursor so stale results are dropped on mismatch.
type hoverResultMsg struct {
	text      string
	path      string
	line, col int
}

// completionDebounce is the delay before firing a completion request.
const completionDebounce = 100 * time.Millisecond

// --- Go-to-Definition / Hover / Go-Back ---

// handleGoToDefinition dispatches an async definition lookup to avoid blocking the TUI.
func (m *AppModel) handleGoToDefinition() (tea.Model, tea.Cmd) {
	if !m.Session.HasLanguageService() {
		return m, nil
	}
	originPath := m.Session.ActiveFile()
	originLine, originCol := m.Editor.CursorPosition()
	return m, func() tea.Msg {
		loc, err := m.Session.LookupDefinition(originLine, originCol)
		if err != nil {
			return goToDefResultMsg{err: err}
		}
		return goToDefResultMsg{
			path:       loc.Path,
			line:       loc.Line,
			col:        loc.Col,
			originPath: originPath,
			originLine: originLine,
			originCol:  originCol,
		}
	}
}

// applyGoToDefinition handles the async definition result on the TUI goroutine.
// Pushes the nav stack, switches files if needed, and moves the cursor.
func (m *AppModel) applyGoToDefinition(msg goToDefResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		slog.Debug("go-to-definition failed", "err", msg.err)
		m.Editor.StatusMsg = msg.err.Error()
		return m, nil
	}

	// Push origin onto nav stack (primitives — no lang.Location in TUI).
	m.Session.PushNav(msg.originPath, msg.originLine, msg.originCol)

	// Switch file if the definition is in a different file.
	if msg.path != m.Session.ActiveFile() {
		if err := m.Session.SwitchTo(msg.path); err != nil {
			m.Session.PopNav() // undo the push
			m.Editor.StatusMsg = err.Error()
			return m, nil
		}
	}

	// Rebuild EditorModel if session switched files.
	if m.activeEditor() != m.Editor.Engine() {
		m.rebuildEditorModel()
		m.refreshDiagnostics(m.Session.ActiveFile())
	}

	if ed := m.activeEditor(); ed != nil {
		ed.MoveCursorTo(msg.line, msg.col)
	}
	slog.Debug("go-to-definition", "path", msg.path, "line", msg.line, "col", msg.col)
	return m, nil
}

// handleGoBack returns to the previous location in the navigation stack.
func (m *AppModel) handleGoBack() (tea.Model, tea.Cmd) {
	loc := m.Session.GoBack()
	if loc == nil {
		return m, nil
	}

	// Session may have switched files — rebuild EditorModel if needed,
	// then place the cursor on the TUI's editor (Session no longer
	// holds a UI cursor).
	if m.activeEditor() != m.Editor.Engine() {
		m.rebuildEditorModel()
		m.refreshDiagnostics(m.Session.ActiveFile())
	}
	if ed := m.activeEditor(); ed != nil {
		ed.MoveCursorTo(loc.Line, loc.Col)
		ed.EnsureCursorVisible()
	}

	slog.Debug("go-back", "path", loc.Path, "line", loc.Line, "col", loc.Col)
	return m, nil
}

// handleHover requests hover info for the symbol under the cursor.
// The LSP request runs asynchronously via a tea.Cmd.
func (m *AppModel) handleHover() (tea.Model, tea.Cmd) {
	if !m.Session.HasLanguageService() {
		return m, nil
	}
	// Dismiss any existing hover.
	m.Editor.DismissHover()

	path := m.Session.ActiveFile()
	line, col := m.Editor.CursorPosition()
	return m, func() tea.Msg {
		text, err := m.Session.HoverInfo(line, col)
		if err != nil {
			slog.Debug("hover failed", "err", err)
			return hoverResultMsg{}
		}
		return hoverResultMsg{text: text, path: path, line: line, col: col}
	}
}

// handleHoverResult displays hover text only if the result is still
// relevant (path + cursor unchanged since the request was dispatched).
func (m *AppModel) handleHoverResult(msg hoverResultMsg) (tea.Model, tea.Cmd) {
	curLine, curCol := m.Editor.CursorPosition()
	if msg.text != "" &&
		msg.path == m.Session.ActiveFile() &&
		msg.line == curLine &&
		msg.col == curCol {
		m.Editor.ShowHover(msg.text)
	}
	return m, nil
}

// --- Autocomplete ---

// scheduleCompletion starts a debounced completion request. Called after
// typing a character. The tick carries a snapshot of path+cursor so stale
// ticks are dropped.
func (m *AppModel) scheduleCompletion() tea.Cmd {
	if !m.Session.HasLanguageService() {
		return nil
	}
	path := m.Session.ActiveFile()
	var line, col int
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		// Map overlay cursor to buffer position for the LSP request.
		// The overlay replaces buffer lines StartLine..EndLine, so the
		// overlay cursor line maps to StartLine + overlayCursorLine.
		oe := m.Editor.Overlay.Editor
		line = m.Editor.Overlay.StartLine + oe.CursorLine
		col = oe.CursorCol
	} else {
		line, col = m.Editor.CursorPosition()
	}
	return tea.Tick(completionDebounce, func(_ time.Time) tea.Msg {
		return completionTickMsg{path: path, line: line, col: col}
	})
}

// handleCompletionTick fires when the debounce timer expires.
// Drops stale ticks (cursor moved since scheduling). Dispatches async request.
func (m *AppModel) handleCompletionTick(msg completionTickMsg) (tea.Model, tea.Cmd) {
	// Drop if cursor moved since the tick was scheduled.
	// Compare against the right cursor (overlay or buffer).
	var curLine, curCol int
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		oe := m.Editor.Overlay.Editor
		curLine = m.Editor.Overlay.StartLine + oe.CursorLine
		curCol = oe.CursorCol
	} else {
		curLine, curCol = m.Editor.CursorPosition()
	}
	if msg.path != m.Session.ActiveFile() ||
		msg.line != curLine ||
		msg.col != curCol {
		return m, nil
	}
	line, col := msg.line, msg.col
	path := msg.path

	// Capture content snapshots on the TUI goroutine (no race).
	// For overlay editing, session needs both merged and original content
	// to temporarily sync the proposed code to LSP and revert afterward.
	var tempContent, originalContent string
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		originalContent = m.Editor.Content()
		tempContent = m.Editor.Overlay.MergedContent(m.Editor.Engine())
	}

	return m, func() tea.Msg {
		var result *lang.CompletionResult
		var err error

		if tempContent != "" {
			// Overlay: sync/query/revert atomically inside session.
			result, err = m.Session.RequestCompletionInContext(path, tempContent, originalContent, line, col)
		} else {
			result, err = m.Session.RequestCompletion(path, line, col)
		}

		if err != nil {
			slog.Debug("completion request failed", "path", path, "line", line, "col", col, "err", err)
			return completionResultMsg{}
		}
		if result == nil {
			return completionResultMsg{}
		}
		return completionResultMsg{
			items:        result.Items,
			isIncomplete: result.IsIncomplete,
			path:         path,
			line:         line,
			col:          col,
		}
	}
}

// handleCompletionResult shows the completion popup only if the result is
// still relevant (path + cursor unchanged since the request was dispatched).
// Cursor sourcing follows the same overlay-aware logic as the request side.
func (m *AppModel) handleCompletionResult(msg completionResultMsg) (tea.Model, tea.Cmd) {
	var curLine, curCol int
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		oe := m.Editor.Overlay.Editor
		curLine = m.Editor.Overlay.StartLine + oe.CursorLine
		curCol = oe.CursorCol
	} else {
		curLine, curCol = m.Editor.CursorPosition()
	}
	if msg.path == m.Session.ActiveFile() &&
		msg.line == curLine &&
		msg.col == curCol &&
		len(msg.items) > 0 {
		m.Editor.Completion.Show(msg.items, msg.line, msg.col)
	}
	return m, nil
}
