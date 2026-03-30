package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/mattn/go-runewidth"
)

// Find bar styles — hoisted to package level to avoid per-frame allocation.
var (
	findBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("252"))
	findInputStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("238")).
			Foreground(lipgloss.Color("255"))
	findLabelStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("245"))
	findCountStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("214"))
	findNoMatchStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("236")).
				Foreground(lipgloss.Color("9"))

	// Match highlight styles used in renderNormalLine.
	findMatchStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("136")).
			Foreground(lipgloss.Color("0"))
	findCurrentMatchStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("214")).
				Foreground(lipgloss.Color("0"))

	// Cursor style for the active input field.
	findCursorStyle = lipgloss.NewStyle().Reverse(true)
)

// FindBar manages the find (and optional replace) overlay for the editor.
type FindBar struct {
	Active    bool
	Query     []rune
	CursorPos int // cursor position within Query

	ReplaceMode   bool
	ReplaceQuery  []rune
	ReplaceCursor int
	ReplaceActive bool // true when replace field has focus

	Matches       []editor.FindMatch
	CurrentMatch  int // index into Matches, -1 if none
	CaseSensitive bool

	// eng is a reference to the editor for running searches.
	eng *editor.Editor

	// savedCursorLine/Col are the cursor position when find was opened,
	// used to restore position if the user cancels without navigating.
	savedCursorLine int
	savedCursorCol  int
	navigated       bool // set true when the user navigates to a match
}

// Open activates the find bar with an optional initial query.
// If the editor has a selection, it is used as the initial query.
func (f *FindBar) Open(eng *editor.Editor, replace bool) {
	f.eng = eng
	f.Active = true
	f.ReplaceMode = replace
	f.ReplaceActive = false
	f.navigated = false
	f.savedCursorLine = eng.CursorLine
	f.savedCursorCol = eng.CursorCol

	// Reset query and cursor state unconditionally.
	f.Query = f.Query[:0]
	f.CursorPos = 0
	f.ReplaceQuery = f.ReplaceQuery[:0]
	f.ReplaceCursor = 0

	// Pre-fill from selection if available.
	if eng.SelectionActive {
		sel := eng.SelectedText()
		// Only single-line selections make sense as find queries.
		if !strings.Contains(sel, "\n") {
			f.Query = []rune(sel)
			f.CursorPos = len(f.Query)
		}
	}

	f.search()
}

// Close deactivates the find bar and clears match state.
// If the user did not navigate to any match (navigated == false),
// the cursor is restored to the position when find was opened.
func (f *FindBar) Close() {
	if !f.navigated && f.eng != nil {
		f.eng.MoveCursorTo(f.savedCursorLine, f.savedCursorCol)
		f.eng.EnsureCursorVisible()
	}
	f.Active = false
	f.Matches = nil
	f.CurrentMatch = -1
	f.Query = f.Query[:0]
	f.CursorPos = 0
	f.ReplaceQuery = f.ReplaceQuery[:0]
	f.ReplaceCursor = 0
	f.ReplaceActive = false
	f.eng = nil
}

// search runs the find query against the buffer and updates matches.
func (f *FindBar) search() {
	if f.eng == nil {
		return
	}
	f.Matches = f.eng.FindAll(string(f.Query), f.CaseSensitive)
	if len(f.Matches) == 0 {
		f.CurrentMatch = -1
		return
	}
	// Find the nearest match at or after the cursor.
	f.CurrentMatch = 0
	for i, m := range f.Matches {
		if m.Line > f.eng.CursorLine || (m.Line == f.eng.CursorLine && m.Col >= f.eng.CursorCol) {
			f.CurrentMatch = i
			break
		}
	}
	f.jumpToCurrentMatch()
}

// NextMatch moves to the next match, wrapping around.
func (f *FindBar) NextMatch() {
	if len(f.Matches) == 0 {
		return
	}
	f.CurrentMatch = (f.CurrentMatch + 1) % len(f.Matches)
	f.jumpToCurrentMatch()
	f.navigated = true
}

// PrevMatch moves to the previous match, wrapping around.
func (f *FindBar) PrevMatch() {
	if len(f.Matches) == 0 {
		return
	}
	f.CurrentMatch--
	if f.CurrentMatch < 0 {
		f.CurrentMatch = len(f.Matches) - 1
	}
	f.jumpToCurrentMatch()
	f.navigated = true
}

// jumpToCurrentMatch moves the editor cursor to the current match.
func (f *FindBar) jumpToCurrentMatch() {
	if f.CurrentMatch < 0 || f.CurrentMatch >= len(f.Matches) {
		return
	}
	m := f.Matches[f.CurrentMatch]
	f.eng.ClearSelection()
	f.eng.MoveCursorTo(m.Line, m.Col)
	f.eng.EnsureCursorVisible()
}

// ReplaceCurrent replaces the current match with the replace query.
func (f *FindBar) ReplaceCurrent() {
	if f.CurrentMatch < 0 || f.CurrentMatch >= len(f.Matches) || !f.ReplaceMode {
		return
	}
	m := f.Matches[f.CurrentMatch]
	f.eng.ReplaceRange(m.Line, m.Col, m.Len, string(f.ReplaceQuery))
	f.navigated = true
	f.search()
}

// ReplaceAll replaces all matches with the replace query.
func (f *FindBar) ReplaceAll() {
	if len(f.Matches) == 0 || !f.ReplaceMode {
		return
	}
	replaceText := string(f.ReplaceQuery)
	// Replace from last match to first to preserve earlier positions.
	for i := len(f.Matches) - 1; i >= 0; i-- {
		m := f.Matches[i]
		f.eng.ReplaceRange(m.Line, m.Col, m.Len, replaceText)
	}
	f.navigated = true
	f.search()
}

// Update handles key events when the find bar is active.
// Returns a tea.Cmd if any, and whether the event was consumed.
func (f *FindBar) Update(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch msg.Code {
	case tea.KeyEscape:
		f.Close()
		return nil, true

	case tea.KeyEnter:
		if f.ReplaceActive {
			// Enter in replace field: replace current match.
			f.ReplaceCurrent()
			return nil, true
		}
		if msg.Mod&tea.ModShift != 0 {
			f.PrevMatch()
		} else if msg.Mod&tea.ModCtrl != 0 && msg.Mod&tea.ModAlt != 0 {
			// Ctrl+Alt+Enter: replace all
			f.ReplaceAll()
		} else {
			f.NextMatch()
		}
		return nil, true

	case tea.KeyTab:
		if f.ReplaceMode {
			f.ReplaceActive = !f.ReplaceActive
			return nil, true
		}

	case tea.KeyBackspace:
		if f.ReplaceActive {
			if len(f.ReplaceQuery) > 0 && f.ReplaceCursor > 0 {
				f.ReplaceQuery = append(f.ReplaceQuery[:f.ReplaceCursor-1], f.ReplaceQuery[f.ReplaceCursor:]...)
				f.ReplaceCursor--
			}
		} else {
			if len(f.Query) > 0 && f.CursorPos > 0 {
				f.Query = append(f.Query[:f.CursorPos-1], f.Query[f.CursorPos:]...)
				f.CursorPos--
				f.search()
			}
		}
		return nil, true

	case tea.KeyLeft:
		if f.ReplaceActive {
			if f.ReplaceCursor > 0 {
				f.ReplaceCursor--
			}
		} else {
			if f.CursorPos > 0 {
				f.CursorPos--
			}
		}
		return nil, true

	case tea.KeyRight:
		if f.ReplaceActive {
			if f.ReplaceCursor < len(f.ReplaceQuery) {
				f.ReplaceCursor++
			}
		} else {
			if f.CursorPos < len(f.Query) {
				f.CursorPos++
			}
		}
		return nil, true

	case tea.KeyHome:
		if f.ReplaceActive {
			f.ReplaceCursor = 0
		} else {
			f.CursorPos = 0
		}
		return nil, true

	case tea.KeyEnd:
		if f.ReplaceActive {
			f.ReplaceCursor = len(f.ReplaceQuery)
		} else {
			f.CursorPos = len(f.Query)
		}
		return nil, true
	}

	// Alt+C: toggle case sensitivity
	if msg.Code == 'c' && msg.Mod == tea.ModAlt {
		f.CaseSensitive = !f.CaseSensitive
		f.search()
		return nil, true
	}

	// Alt+R: toggle replace mode
	if msg.Code == 'r' && msg.Mod == tea.ModAlt {
		f.ReplaceMode = !f.ReplaceMode
		if !f.ReplaceMode {
			f.ReplaceActive = false
		}
		return nil, true
	}

	// Printable input
	if msg.Text != "" {
		runes := []rune(msg.Text)
		if f.ReplaceActive {
			f.ReplaceQuery = slices.Insert(f.ReplaceQuery, f.ReplaceCursor, runes...)
			f.ReplaceCursor += len(runes)
		} else {
			f.Query = slices.Insert(f.Query, f.CursorPos, runes...)
			f.CursorPos += len(runes)
			f.search()
		}
		return nil, true
	}

	return nil, true // consume all keys when find bar is active
}

// Height returns the number of rows the find bar occupies.
func (f *FindBar) Height() int {
	if !f.Active {
		return 0
	}
	if f.ReplaceMode {
		return 2
	}
	return 1
}

// Render returns the find bar line(s) at the given width.
func (f *FindBar) Render(width int) []string {
	if !f.Active || width < 10 {
		return nil
	}

	findLine := f.renderFindLine(width)
	if !f.ReplaceMode {
		return []string{findLine}
	}
	replaceLine := f.renderReplaceLine(width)
	return []string{findLine, replaceLine}
}

// renderInput renders a text input field with a visible cursor.
// fieldWidth is in display columns (accounts for wide characters).
// showCursor controls whether the cursor is drawn (only for the focused field).
func renderInput(text []rune, cursorPos, fieldWidth int, showCursor bool) string {
	// Append a trailing space so the cursor has somewhere to sit at the end.
	display := append(text, ' ')

	// Scroll the visible window so the cursor is always visible.
	// Walk backwards from cursorPos to find the start index that fits fieldWidth columns.
	start := 0
	widthToCursor := 0
	for i := 0; i <= cursorPos && i < len(display); i++ {
		widthToCursor += runewidth.RuneWidth(display[i])
	}
	if widthToCursor > fieldWidth {
		// Scroll forward until the cursor fits.
		cols := 0
		for i := 0; i < len(display); i++ {
			cols += runewidth.RuneWidth(display[i])
			if cols > widthToCursor-fieldWidth {
				start = i + 1
				break
			}
		}
	}

	// Render characters from start, tracking column width.
	var b strings.Builder
	usedCols := 0
	cursorRendered := false
	for i := start; i < len(display) && usedCols < fieldWidth; i++ {
		ch := string(display[i])
		w := runewidth.RuneWidth(display[i])
		if usedCols+w > fieldWidth {
			break // wide char would overflow
		}
		if showCursor && i == cursorPos {
			b.WriteString(findCursorStyle.Render(ch))
			cursorRendered = true
		} else {
			b.WriteString(findInputStyle.Render(ch))
		}
		usedCols += w
	}
	// Pad remaining columns.
	for usedCols < fieldWidth {
		if showCursor && !cursorRendered && usedCols == fieldWidth-1 {
			b.WriteString(findCursorStyle.Render(" "))
			cursorRendered = true
		} else {
			b.WriteString(findInputStyle.Render(" "))
		}
		usedCols++
	}
	return b.String()
}

// renderFindLine renders the main find bar.
func (f *FindBar) renderFindLine(width int) string {
	const labelText = " Find: "
	const caseText = " [Aa]"
	labelW := runewidth.StringWidth(labelText)
	caseW := runewidth.StringWidth(caseText)

	// Compute count text (raw, before styling).
	var countText string
	if len(f.Query) == 0 {
		countText = ""
	} else if len(f.Matches) == 0 {
		countText = " No matches "
	} else {
		countText = fmt.Sprintf(" %d of %d ", f.CurrentMatch+1, len(f.Matches))
	}
	countW := runewidth.StringWidth(countText)

	// Input field gets remaining space.
	inputW := width - labelW - countW - caseW
	if inputW < 5 {
		inputW = 5
	}

	// Cursor shows in find field when replace field is NOT focused.
	showCursor := !f.ReplaceActive
	inputField := renderInput(f.Query, f.CursorPos, inputW, showCursor)

	// Render styled parts.
	label := findLabelStyle.Render(labelText)

	var countStyled string
	if len(f.Query) == 0 {
		countStyled = ""
	} else if len(f.Matches) == 0 {
		countStyled = findNoMatchStyle.Render(countText)
	} else {
		countStyled = findCountStyle.Render(countText)
	}

	caseStyled := findLabelStyle.Render(caseText)
	if f.CaseSensitive {
		caseStyled = findCountStyle.Render(caseText)
	}

	// Compute used width from raw texts, then pad.
	usedW := labelW + inputW + countW + caseW
	bar := label + inputField + countStyled + caseStyled
	if pad := width - usedW; pad > 0 {
		bar += findBarStyle.Render(strings.Repeat(" ", pad))
	}

	return bar
}

// renderReplaceLine renders the replace bar (second row).
func (f *FindBar) renderReplaceLine(width int) string {
	const labelText = " With: "
	labelW := runewidth.StringWidth(labelText)

	inputW := width - labelW
	if inputW < 5 {
		inputW = 5
	}

	label := findLabelStyle.Render(labelText)
	inputField := renderInput(f.ReplaceQuery, f.ReplaceCursor, inputW, f.ReplaceActive)

	bar := label + inputField
	if pad := width - labelW - inputW; pad > 0 {
		bar += findBarStyle.Render(strings.Repeat(" ", pad))
	}

	return bar
}

// IsMatchAt returns true if a find match covers (line, col) in buffer coordinates.
// Returns isCurrent to distinguish the currently focused match.
func (f *FindBar) IsMatchAt(line, bufCol int) (isMatch, isCurrent bool) {
	if !f.Active || len(f.Matches) == 0 {
		return false, false
	}
	for i, m := range f.Matches {
		if m.Line == line && bufCol >= m.Col && bufCol < m.Col+m.Len {
			return true, i == f.CurrentMatch
		}
		if m.Line > line {
			break // matches are sorted, no need to check further
		}
	}
	return false, false
}
