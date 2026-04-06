package textarea

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

type mockClipboard struct {
	content string
}

func (c *mockClipboard) Read() string         { return c.content }
func (c *mockClipboard) Write(s string) error { c.content = s; return nil }

func newTestArea(text string) *TextArea {
	t := New(40)
	t.SetClipboard(&mockClipboard{})
	if text != "" {
		t.SetContent(text)
	}
	return t
}

func key(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: string(code)}
}

func special(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func shifted(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModShift}
}

func ctrl(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}
}

func altKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModAlt}
}

func altShift(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModAlt | tea.ModShift}
}

// --- Basic Editing ---

func TestInsertAndContent(t *testing.T) {
	ta := newTestArea("")
	ta.Update(key('h'))
	ta.Update(key('i'))

	got := ta.Content()
	if got != "hi" {
		t.Errorf("Content() = %q, want %q", got, "hi")
	}
}

func TestBackspace(t *testing.T) {
	ta := newTestArea("abc")
	ta.Update(special(tea.KeyBackspace))

	if got := ta.Content(); got != "ab" {
		t.Errorf("Content() = %q, want %q", got, "ab")
	}
}

func TestBackspaceAtStart(t *testing.T) {
	ta := newTestArea("")
	ta.cursorLine = 0
	ta.cursorCol = 0
	ta.Update(special(tea.KeyBackspace)) // should not panic

	if got := ta.Content(); got != "" {
		t.Errorf("Content() = %q, want %q", got, "")
	}
}

func TestDeleteForward(t *testing.T) {
	ta := newTestArea("abc")
	ta.cursorCol = 1 // between 'a' and 'b'
	ta.Update(special(tea.KeyDelete))

	if got := ta.Content(); got != "ac" {
		t.Errorf("Content() = %q, want %q", got, "ac")
	}
}

func TestInsertNewline(t *testing.T) {
	ta := newTestArea("ab")
	ta.cursorCol = 1 // between 'a' and 'b'
	ta.Update(shifted(tea.KeyEnter))

	if got := ta.Content(); got != "a\nb" {
		t.Errorf("Content() = %q, want %q", got, "a\nb")
	}
	if ta.cursorLine != 1 || ta.cursorCol != 0 {
		t.Errorf("cursor = (%d,%d), want (1,0)", ta.cursorLine, ta.cursorCol)
	}
}

func TestBackspaceAcrossLines(t *testing.T) {
	ta := newTestArea("a\nb")
	ta.cursorLine = 1
	ta.cursorCol = 0
	ta.Update(special(tea.KeyBackspace))

	if got := ta.Content(); got != "ab" {
		t.Errorf("Content() = %q, want %q", got, "ab")
	}
	if ta.cursorLine != 0 || ta.cursorCol != 1 {
		t.Errorf("cursor = (%d,%d), want (0,1)", ta.cursorLine, ta.cursorCol)
	}
}

func TestDeleteForwardAcrossLines(t *testing.T) {
	ta := newTestArea("a\nb")
	ta.cursorLine = 0
	ta.cursorCol = 1
	ta.Update(special(tea.KeyDelete))

	if got := ta.Content(); got != "ab" {
		t.Errorf("Content() = %q, want %q", got, "ab")
	}
}

// --- Cursor Movement ---

func TestMoveLeftRight(t *testing.T) {
	ta := newTestArea("abc")
	ta.cursorCol = 3

	ta.Update(special(tea.KeyLeft))
	if ta.cursorCol != 2 {
		t.Errorf("after left: col = %d, want 2", ta.cursorCol)
	}

	ta.Update(special(tea.KeyRight))
	if ta.cursorCol != 3 {
		t.Errorf("after right: col = %d, want 3", ta.cursorCol)
	}
}

func TestMoveLeftWrapsToPreLine(t *testing.T) {
	ta := newTestArea("ab\ncd")
	ta.cursorLine = 1
	ta.cursorCol = 0

	ta.Update(special(tea.KeyLeft))
	if ta.cursorLine != 0 || ta.cursorCol != 2 {
		t.Errorf("cursor = (%d,%d), want (0,2)", ta.cursorLine, ta.cursorCol)
	}
}

func TestMoveRightWrapsToNextLine(t *testing.T) {
	ta := newTestArea("ab\ncd")
	ta.cursorLine = 0
	ta.cursorCol = 2

	ta.Update(special(tea.KeyRight))
	if ta.cursorLine != 1 || ta.cursorCol != 0 {
		t.Errorf("cursor = (%d,%d), want (1,0)", ta.cursorLine, ta.cursorCol)
	}
}

func TestMoveUpDown(t *testing.T) {
	ta := newTestArea("abc\ndef")
	ta.cursorLine = 0
	ta.cursorCol = 1

	ta.Update(special(tea.KeyDown))
	if ta.cursorLine != 1 || ta.cursorCol != 1 {
		t.Errorf("after down: cursor = (%d,%d), want (1,1)", ta.cursorLine, ta.cursorCol)
	}

	ta.Update(special(tea.KeyUp))
	if ta.cursorLine != 0 || ta.cursorCol != 1 {
		t.Errorf("after up: cursor = (%d,%d), want (0,1)", ta.cursorLine, ta.cursorCol)
	}
}

func TestHomeEnd(t *testing.T) {
	ta := newTestArea("hello world")
	ta.cursorCol = 5

	ta.Update(special(tea.KeyHome))
	if ta.cursorCol != 0 {
		t.Errorf("after home: col = %d, want 0", ta.cursorCol)
	}

	ta.Update(special(tea.KeyEnd))
	if ta.cursorCol != 11 {
		t.Errorf("after end: col = %d, want 11", ta.cursorCol)
	}
}

// --- Word Navigation ---

func TestWordRight(t *testing.T) {
	ta := newTestArea("hello world")
	ta.cursorCol = 0

	ta.Update(altKey(tea.KeyRight))
	// Should be past "hello " at position 6
	if ta.cursorCol != 6 {
		t.Errorf("after word right: col = %d, want 6", ta.cursorCol)
	}
}

func TestWordLeft(t *testing.T) {
	ta := newTestArea("hello world")
	ta.cursorCol = 11

	ta.Update(altKey(tea.KeyLeft))
	// Should be at start of "world" at position 6
	if ta.cursorCol != 6 {
		t.Errorf("after word left: col = %d, want 6", ta.cursorCol)
	}
}

// --- Selection ---

func TestShiftArrowSelection(t *testing.T) {
	ta := newTestArea("hello")
	ta.cursorCol = 0

	// Select "hel" by pressing Shift+Right 3 times.
	ta.Update(shifted(tea.KeyRight))
	ta.Update(shifted(tea.KeyRight))
	ta.Update(shifted(tea.KeyRight))

	if !ta.selActive {
		t.Fatal("selection should be active")
	}
	if got := ta.SelectedText(); got != "hel" {
		t.Errorf("SelectedText() = %q, want %q", got, "hel")
	}
}

func TestSelectAll(t *testing.T) {
	ta := newTestArea("hello\nworld")
	ta.Update(ctrl('a'))

	if got := ta.SelectedText(); got != "hello\nworld" {
		t.Errorf("SelectedText() = %q, want %q", got, "hello\nworld")
	}
}

func TestTypingReplacesSelection(t *testing.T) {
	ta := newTestArea("hello")
	ta.Update(ctrl('a'))
	ta.Update(key('x'))

	if got := ta.Content(); got != "x" {
		t.Errorf("Content() = %q, want %q", got, "x")
	}
}

func TestBackspaceDeletesSelection(t *testing.T) {
	ta := newTestArea("hello")
	ta.cursorCol = 0
	ta.Update(shifted(tea.KeyRight))
	ta.Update(shifted(tea.KeyRight))
	ta.Update(special(tea.KeyBackspace))

	if got := ta.Content(); got != "llo" {
		t.Errorf("Content() = %q, want %q", got, "llo")
	}
}

// --- Word Selection ---

func TestAltShiftRightSelection(t *testing.T) {
	ta := newTestArea("hello world")
	ta.cursorCol = 0

	ta.Update(altShift(tea.KeyRight))
	if got := ta.SelectedText(); got != "hello " {
		t.Errorf("SelectedText() = %q, want %q", got, "hello ")
	}
}

// --- Clipboard ---

func TestCopyPaste(t *testing.T) {
	cb := &mockClipboard{}
	ta := newTestArea("hello")
	ta.SetClipboard(cb)

	// Select all and copy.
	ta.Update(ctrl('a'))
	ta.Update(ctrl('c'))

	if cb.content != "hello" {
		t.Errorf("clipboard = %q, want %q", cb.content, "hello")
	}

	// Move to end, paste.
	ta.clearSelection()
	ta.cursorCol = 5
	ta.Update(ctrl('v'))

	if got := ta.Content(); got != "hellohello" {
		t.Errorf("Content() = %q, want %q", got, "hellohello")
	}
}

func TestCut(t *testing.T) {
	cb := &mockClipboard{}
	ta := newTestArea("hello world")
	ta.SetClipboard(cb)

	// Select "hello" and cut.
	ta.cursorCol = 0
	for range 5 {
		ta.Update(shifted(tea.KeyRight))
	}
	ta.Update(ctrl('x'))

	if cb.content != "hello" {
		t.Errorf("clipboard = %q, want %q", cb.content, "hello")
	}
	if got := ta.Content(); got != " world" {
		t.Errorf("Content() = %q, want %q", got, " world")
	}
}

func TestPastePreservesNewlines(t *testing.T) {
	cb := &mockClipboard{content: "line1\nline2"}
	ta := newTestArea("")
	ta.SetClipboard(cb)

	ta.Update(ctrl('v'))

	if got := ta.Content(); got != "line1\nline2" {
		t.Errorf("Content() = %q, want %q", got, "line1\nline2")
	}
	if ta.cursorLine != 1 || ta.cursorCol != 5 {
		t.Errorf("cursor = (%d,%d), want (1,5)", ta.cursorLine, ta.cursorCol)
	}
}

// --- Submit/Cancel ---

func TestEnterSubmits(t *testing.T) {
	ta := newTestArea("test")
	cmd := ta.Update(special(tea.KeyEnter))

	if cmd == nil {
		t.Fatal("expected a command from Enter")
	}
	msg := cmd()
	if _, ok := msg.(SubmitMsg); !ok {
		t.Errorf("expected SubmitMsg, got %T", msg)
	}
}

func TestEscapeCancels(t *testing.T) {
	ta := newTestArea("test")
	cmd := ta.Update(special(tea.KeyEscape))

	if cmd == nil {
		t.Fatal("expected a command from Escape")
	}
	msg := cmd()
	if _, ok := msg.(CancelMsg); !ok {
		t.Errorf("expected CancelMsg, got %T", msg)
	}
}

// --- Reset ---

func TestReset(t *testing.T) {
	ta := newTestArea("some content")
	ta.Reset()

	if got := ta.Content(); got != "" {
		t.Errorf("Content() = %q, want empty", got)
	}
	if ta.cursorLine != 0 || ta.cursorCol != 0 {
		t.Errorf("cursor = (%d,%d), want (0,0)", ta.cursorLine, ta.cursorCol)
	}
}

// --- SetContent ---

func TestSetContent(t *testing.T) {
	ta := newTestArea("")
	ta.SetContent("line1\nline2\nline3")

	if got := ta.Content(); got != "line1\nline2\nline3" {
		t.Errorf("Content() = %q", got)
	}
	// Cursor at end of last line.
	if ta.cursorLine != 2 || ta.cursorCol != 5 {
		t.Errorf("cursor = (%d,%d), want (2,5)", ta.cursorLine, ta.cursorCol)
	}
}

// --- MaxBytes ---

func TestMaxBytesEnforced(t *testing.T) {
	ta := New(40)
	ta.maxBytes = 5

	for _, r := range "abcdef" {
		ta.Update(key(r))
	}

	got := ta.Content()
	if len(got) > 5 {
		t.Errorf("Content() = %q (len %d), should not exceed 5 bytes", got, len(got))
	}
}

// --- Edge Cases ---

func TestMoveUpAtTopNoop(t *testing.T) {
	ta := newTestArea("abc")
	ta.cursorLine = 0
	ta.cursorCol = 1

	ta.Update(special(tea.KeyUp))
	if ta.cursorLine != 0 || ta.cursorCol != 1 {
		t.Errorf("cursor = (%d,%d), want (0,1)", ta.cursorLine, ta.cursorCol)
	}
}

func TestMoveDownAtBottomNoop(t *testing.T) {
	ta := newTestArea("abc")
	ta.cursorLine = 0
	ta.cursorCol = 1

	ta.Update(special(tea.KeyDown))
	if ta.cursorLine != 0 || ta.cursorCol != 1 {
		t.Errorf("cursor = (%d,%d), want (0,1)", ta.cursorLine, ta.cursorCol)
	}
}

func TestMoveRightAtEndNoop(t *testing.T) {
	ta := newTestArea("abc")
	// Cursor already at end (SetContent places it there).

	ta.Update(special(tea.KeyRight))
	if ta.cursorCol != 3 {
		t.Errorf("col = %d, want 3", ta.cursorCol)
	}
}

func TestMoveLeftAtStartNoop(t *testing.T) {
	ta := newTestArea("abc")
	ta.cursorLine = 0
	ta.cursorCol = 0

	ta.Update(special(tea.KeyLeft))
	if ta.cursorCol != 0 {
		t.Errorf("col = %d, want 0", ta.cursorCol)
	}
}

// TestBracketPastePreservesNewlines simulates terminal bracket paste
// (Cmd+V) which delivers multi-line text via msg.Text on a KeyPressMsg.
func TestBracketPastePreservesNewlines(t *testing.T) {
	ta := newTestArea("")

	// Simulate bracket paste: terminal delivers entire pasted text as msg.Text.
	ta.Update(tea.KeyPressMsg{Text: "line1\nline2\nline3"})

	if got := ta.Content(); got != "line1\nline2\nline3" {
		t.Errorf("Content() = %q, want %q", got, "line1\nline2\nline3")
	}
	if ta.cursorLine != 2 || ta.cursorCol != 5 {
		t.Errorf("cursor = (%d,%d), want (2,5)", ta.cursorLine, ta.cursorCol)
	}
}

// --- Mouse ---

func TestHandleClickPositionsCursor(t *testing.T) {
	ta := newTestArea("hello world")
	ta.cursorCol = 0

	// Click at visual row 0, cell col 6 → should land on 'w'
	ta.HandleClick(0, 6)
	if ta.cursorLine != 0 || ta.cursorCol != 6 {
		t.Errorf("cursor = (%d,%d), want (0,6)", ta.cursorLine, ta.cursorCol)
	}
	if ta.selActive {
		t.Error("selection should not be active after click")
	}
}

func TestHandleClickPastEnd(t *testing.T) {
	ta := newTestArea("hi")
	ta.HandleClick(0, 20) // past end of "hi"
	if ta.cursorCol != 2 {
		t.Errorf("col = %d, want 2", ta.cursorCol)
	}
}

func TestHandleDragCreatesSelection(t *testing.T) {
	ta := newTestArea("hello world")

	ta.HandleClick(0, 0)
	ta.HandleDrag(0, 5)

	if !ta.selActive {
		t.Fatal("selection should be active after drag")
	}
	if got := ta.SelectedText(); got != "hello" {
		t.Errorf("SelectedText() = %q, want %q", got, "hello")
	}
}

func TestHandleDragMultiline(t *testing.T) {
	ta := newTestArea("abc\ndef")

	ta.HandleClick(0, 1) // after 'a'
	ta.HandleDrag(1, 2)  // after 'e' on second line

	if !ta.selActive {
		t.Fatal("selection should be active")
	}
	if got := ta.SelectedText(); got != "bc\nde" {
		t.Errorf("SelectedText() = %q, want %q", got, "bc\nde")
	}
}

func TestClickClearsSelection(t *testing.T) {
	ta := newTestArea("hello")
	ta.Update(ctrl('a')) // select all
	if !ta.selActive {
		t.Fatal("selection should be active after Ctrl+A")
	}

	ta.HandleClick(0, 2) // click clears it
	if ta.selActive {
		t.Error("selection should be cleared after click")
	}
}

func TestSpaceKey(t *testing.T) {
	ta := newTestArea("")
	ta.Update(key('a'))
	ta.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	ta.Update(key('b'))

	if got := ta.Content(); got != "a b" {
		t.Errorf("Content() = %q, want %q", got, "a b")
	}
}
