// Package ui provides the Bubble Tea TUI components for junto.
package ui

import (
	"fmt"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/highlight"
	"github.com/mattn/go-runewidth"
)

// EditorModel is the Bubble Tea view for the code editor pane.
// It wraps the engine's Editor (which owns all domain logic)
// and adds only rendering + input mapping.
type EditorModel struct {
	*editor.Editor

	// Transient status message (shown in status bar, cleared on next key)
	StatusMsg string

	// Keymap for action matching (shared with AppModel)
	Keymap *Keymap

	// Shared services (clipboard, etc.)
	Services *Services

	// Internal clipboard buffer
	Clipboard string
}

// NewEditorModel creates an editor model from an engine Editor.
func NewEditorModel(e *editor.Editor, km *Keymap, svc *Services) *EditorModel {
	return &EditorModel{
		Editor:   e,
		Keymap:   km,
		Services: svc,
	}
}

// Update handles key and mouse events for the editor pane. Implements Pane.
func (m *EditorModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return nil
}

// Render renders the editor view. Implements Pane.
func (m *EditorModel) Render() string {
	m.ReparseIfNeeded()

	gutterW := m.GutterWidth()
	contentW := m.ContentWidth()

	vis := m.VisibleLines()
	gutterStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	output := make([]string, m.Height)
	row := 0

	cursorStyle := lipgloss.NewStyle().Reverse(true)
	selectionStyle := lipgloss.NewStyle().Background(lipgloss.Color("24"))

	for i := range vis {
		if row >= m.Height {
			break
		}
		lineIdx := m.ScrollOffset + i
		var line strings.Builder

		if lineIdx >= m.Buf.LineCount() {
			gutterText := fmt.Sprintf("%*s ", gutterW-1, "~")
			line.WriteString(gutterStyle.Render(gutterText))
			line.WriteString(strings.Repeat(" ", contentW))
		} else {
			gutterText := fmt.Sprintf("%*d ", gutterW-1, lineIdx+1)
			line.WriteString(gutterStyle.Render(gutterText))

			// Build tab→display column mapping and expanded line
			rawRunes := []rune(m.Buf.LineText(lineIdx))
			var expandedRunes []rune
			// bufToDisp maps buffer col → display col
			bufToDisp := make([]int, len(rawRunes)+1)
			dispCol := 0
			for bi, r := range rawRunes {
				bufToDisp[bi] = dispCol
				if r == '\t' {
					expandedRunes = append(expandedRunes, ' ', ' ', ' ', ' ')
					dispCol += 4
				} else {
					expandedRunes = append(expandedRunes, r)
					dispCol++
				}
			}
			bufToDisp[len(rawRunes)] = dispCol

			displayed := make([]rune, contentW)
			for j := range displayed {
				displayed[j] = ' '
			}
			for j := 0; j < len(expandedRunes) && j < contentW; j++ {
				displayed[j] = expandedRunes[j]
			}

			// Map cursor and selection to display coords
			displayCursorCol := -1
			if lineIdx == m.CursorLine && m.CursorCol >= 0 && m.CursorCol <= len(rawRunes) {
				displayCursorCol = bufToDisp[m.CursorCol]
				// Clamp to last visible column so cursor is renderable at EOL
				if displayCursorCol >= contentW && contentW > 0 {
					displayCursorCol = contentW - 1
				}
			}

			charStyles := make([]lipgloss.Style, contentW)
			if m.Highlighter != nil {
				tokens := m.Highlighter.HighlightLine(lineIdx)
				for _, tok := range tokens {
					// Token cols are in buffer coords — convert to display
					dStart := 0
					if tok.Col < len(bufToDisp) {
						dStart = bufToDisp[tok.Col]
					}
					dEnd := dStart + tok.Len
					tokEnd := tok.Col + tok.Len
					if tokEnd < len(bufToDisp) {
						dEnd = bufToDisp[tokEnd]
					}
					style := styleForTokenKind(tok.Kind)
					for j := dStart; j < dEnd && j < contentW; j++ {
						charStyles[j] = style
					}
				}
			}

			// Precompute inverse mapping: display col → buffer col (O(1) lookup in render loop)
			// bufToDisp has len(rawRunes)+1 entries; the last maps to the EOL position.
			dispToBuf := make([]int, contentW)
			if m.SelectionActive {
				bufCol := 0
				for j := range contentW {
					// Advance bufCol while the next buffer position maps to this display col or earlier.
					// Allow advancing to len(rawRunes) (EOL) so trailing spaces map correctly.
					for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= j {
						bufCol++
					}
					dispToBuf[j] = bufCol
				}
			}

			for j := range contentW {
				ch := string(displayed[j])
				isCursor := j == displayCursorCol
				isSel := m.SelectionActive && m.IsSelected(lineIdx, dispToBuf[j])

				if isCursor {
					line.WriteString(cursorStyle.Render(ch))
				} else if isSel {
					line.WriteString(selectionStyle.Render(ch))
				} else if charStyles[j].GetForeground() != nil {
					line.WriteString(charStyles[j].Render(ch))
				} else {
					line.WriteString(ch)
				}
			}
		}

		output[row] = line.String()
		row++
	}

	// Fill any remaining rows (if vis < Height-1)
	for row < m.Height-1 {
		output[row] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
		row++
	}

	// Status bar (last row)
	if row < m.Height {
		output[row] = m.renderStatusBar()
	}

	return strings.Join(output, "\n")
}

func (m *EditorModel) renderStatusBar() string {
	statusStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("62")).
		Foreground(lipgloss.Color("230")).
		Bold(true)

	name := m.Buf.Path
	if name == "" {
		name = "[new]"
	}
	modified := ""
	if m.Buf.Modified {
		modified = " [+]"
	}

	left := fmt.Sprintf(" %s%s", name, modified)
	if m.StatusMsg != "" {
		left += "  " + m.StatusMsg
	}
	right := fmt.Sprintf(" %d:%d ", m.CursorLine+1, m.CursorCol+1)

	leftW := runewidth.StringWidth(left)
	rightW := runewidth.StringWidth(right)
	padding := m.Width - leftW - rightW
	if padding < 0 {
		padding = 0
	}

	bar := left + strings.Repeat(" ", padding) + right
	bar = runewidth.Truncate(bar, m.Width, "")

	return statusStyle.Render(bar)
}

func (m *EditorModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	scrollLines := 3

	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp:
		m.ScrollUp(scrollLines)
		return nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown:
		m.ScrollDown(scrollLines)
		return nil
	}

	// Click/drag — local X coordinate (already translated by RegionManager)
	if msg.Button == tea.MouseButtonLeft && msg.Y < m.VisibleLines() {
		gutterW := m.GutterWidth()
		displayCol := msg.X - gutterW
		if displayCol < 0 {
			displayCol = 0
		}
		line := m.ScrollOffset + msg.Y
		// Clamp to valid buffer range before computing buffer column
		var bufCol int
		if line >= m.Buf.LineCount() {
			line = m.Buf.LineCount() - 1
			if line < 0 {
				line = 0
			}
			bufCol = m.Buf.LineLen(line) // click past EOF → end of last line
		} else {
			bufCol = m.DisplayColToBufferCol(line, displayCol)
		}

		switch msg.Action {
		case tea.MouseActionPress:
			m.ClearSelection()
			m.MoveCursorTo(line, bufCol)
			m.SelectionActive = true
			m.SelectStartLine = m.CursorLine
			m.SelectStartCol = m.CursorCol
		case tea.MouseActionMotion:
			if m.SelectionActive {
				m.MoveCursorTo(line, bufCol)
			}
		case tea.MouseActionRelease:
			if m.SelectionActive &&
				m.CursorLine == m.SelectStartLine &&
				m.CursorCol == m.SelectStartCol {
				m.ClearSelection()
			}
		}
	}

	return nil
}

func (m *EditorModel) handleKey(keyMsg tea.KeyMsg) tea.Cmd {

	m.StatusMsg = "" // clear transient status on any key

	isShift := keyMsg.Type == tea.KeyShiftUp || keyMsg.Type == tea.KeyShiftDown ||
		keyMsg.Type == tea.KeyShiftLeft || keyMsg.Type == tea.KeyShiftRight ||
		keyMsg.Type == tea.KeyShiftHome || keyMsg.Type == tea.KeyShiftEnd

	action := m.Keymap.Match(keyMsg)

	switch action {
	case ActionSave:
		if err := m.Save(); err != nil {
			m.StatusMsg = "Save failed: " + err.Error()
		} else {
			m.StatusMsg = "Saved"
		}
		return nil

	case ActionUndo:
		m.Undo()
		return nil

	case ActionRedo:
		m.Redo()
		return nil

	case ActionCopy:
		if m.SelectionActive {
			m.Clipboard = m.SelectedText()
			if err := m.Services.Clipboard.Write(m.Clipboard); err != nil {
				slog.Warn("system clipboard write failed", "err", err)
			}
		}
		return nil

	case ActionCut:
		if m.SelectionActive {
			m.Clipboard = m.SelectedText()
			if err := m.Services.Clipboard.Write(m.Clipboard); err != nil {
				slog.Warn("system clipboard write failed", "err", err)
			}
			m.DeleteSelection()
			m.MarkDirty()
		}
		return nil

	case ActionPaste:
		if sys := m.Services.Clipboard.Read(); sys != "" {
			m.Clipboard = sys
		}
		if m.Clipboard != "" {
			m.PasteText(m.Clipboard)
		}
		return nil

	case ActionSelectAll:
		m.SelectAll()
		return nil
	}

	// Escape (clear selection) — only reached when AppModel has no pending edit
	if keyMsg.Type == tea.KeyEscape {
		m.ClearSelection()
		return nil
	}

	// Navigation and editing
	switch keyMsg.Type {

	// Selection navigation
	case tea.KeyShiftUp:
		m.StartSelection()
		m.MoveCursor(-1, 0)
		return nil
	case tea.KeyShiftDown:
		m.StartSelection()
		m.MoveCursor(1, 0)
		return nil
	case tea.KeyShiftLeft:
		m.StartSelection()
		m.MoveCursor(0, -1)
		return nil
	case tea.KeyShiftRight:
		m.StartSelection()
		m.MoveCursor(0, 1)
		return nil
	case tea.KeyShiftHome:
		m.StartSelection()
		m.Home()
		return nil
	case tea.KeyShiftEnd:
		m.StartSelection()
		m.End()
		return nil

	// Navigation
	case tea.KeyUp:
		m.ClearSelection()
		m.MoveCursor(-1, 0)
		return nil
	case tea.KeyDown:
		m.ClearSelection()
		m.MoveCursor(1, 0)
		return nil
	case tea.KeyLeft:
		m.ClearSelection()
		m.MoveCursor(0, -1)
		return nil
	case tea.KeyRight:
		m.ClearSelection()
		m.MoveCursor(0, 1)
		return nil
	case tea.KeyHome:
		m.ClearSelection()
		m.Home()
		return nil
	case tea.KeyEnd:
		m.ClearSelection()
		m.End()
		return nil
	case tea.KeyPgUp:
		m.ClearSelection()
		m.PageUp()
		return nil
	case tea.KeyPgDown:
		m.ClearSelection()
		m.PageDown()
		return nil

	// Word navigation
	case tea.KeyCtrlRight:
		m.ClearSelection()
		m.WordRight()
		return nil
	case tea.KeyCtrlLeft:
		m.ClearSelection()
		m.WordLeft()
		return nil

	// Editing
	case tea.KeyEnter:
		if m.SelectionActive {
			m.DeleteSelection()
		}
		m.InsertNewline()
		return nil
	case tea.KeyTab:
		if m.SelectionActive {
			m.DeleteSelection()
		}
		m.InsertTab()
		return nil
	case tea.KeyBackspace:
		if m.SelectionActive {
			m.DeleteSelection()
			m.MarkDirty()
		} else {
			m.Backspace()
		}
		return nil
	case tea.KeyDelete:
		if m.SelectionActive {
			m.DeleteSelection()
			m.MarkDirty()
		} else {
			m.DeleteChar()
		}
		return nil

	// Character input (also handles Cmd+V paste on macOS — arrives as multi-char KeyRunes)
	case tea.KeyRunes:
		if len(keyMsg.Runes) > 1 {
			m.PasteText(string(keyMsg.Runes))
		} else {
			if m.SelectionActive {
				m.DeleteSelection()
			}
			for _, r := range keyMsg.Runes {
				m.InsertChar(r)
			}
		}
		return nil
	}

	if !isShift {
		m.ClearSelection()
	}

	return nil
}

// styleForTokenKind maps engine highlight.TokenKind to lipgloss.Style for TUI rendering.
func styleForTokenKind(kind highlight.TokenKind) lipgloss.Style {
	switch kind {
	case highlight.KindKeyword:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("5")) // magenta
	case highlight.KindString:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	case highlight.KindComment:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // gray
	case highlight.KindNumber:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	case highlight.KindType:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	case highlight.KindOperator:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // bright red
	default:
		return lipgloss.NewStyle()
	}
}
