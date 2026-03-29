package editor

import (
	"testing"

	"github.com/latebit-io/junto/engine/buffer"
)

func newLineOpsEditor(content string) *Editor {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	e := New(buf)
	e.SetSize(80, 24)
	return e
}

func bufText(e *Editor) string {
	var s string
	for i := 0; i < e.Buf.LineCount(); i++ {
		if i > 0 {
			s += "\n"
		}
		s += e.Buf.LineText(i)
	}
	return s
}

// --- FileStart / FileEnd ---

func TestFileStart(t *testing.T) {
	e := newLineOpsEditor("line1\nline2\nline3")
	e.CursorLine = 2
	e.CursorCol = 3
	e.FileStart()
	if e.CursorLine != 0 || e.CursorCol != 0 {
		t.Errorf("FileStart: got (%d,%d), want (0,0)", e.CursorLine, e.CursorCol)
	}
}

func TestFileEnd(t *testing.T) {
	e := newLineOpsEditor("line1\nline2\nend")
	e.FileEnd()
	if e.CursorLine != 2 || e.CursorCol != 3 {
		t.Errorf("FileEnd: got (%d,%d), want (2,3)", e.CursorLine, e.CursorCol)
	}
}

// --- DeleteLine ---

func TestDeleteLine_MiddleLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.DeleteLine()
	want := "aaa\nccc"
	if got := bufText(e); got != want {
		t.Errorf("DeleteLine middle: got %q, want %q", got, want)
	}
	if e.CursorLine != 1 {
		t.Errorf("cursor line: got %d, want 1", e.CursorLine)
	}
}

func TestDeleteLine_LastLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb")
	e.CursorLine = 1
	e.DeleteLine()
	want := "aaa"
	if got := bufText(e); got != want {
		t.Errorf("DeleteLine last: got %q, want %q", got, want)
	}
	if e.CursorLine != 0 {
		t.Errorf("cursor line: got %d, want 0", e.CursorLine)
	}
}

func TestDeleteLine_OnlyLine(t *testing.T) {
	e := newLineOpsEditor("only")
	e.DeleteLine()
	want := ""
	if got := bufText(e); got != want {
		t.Errorf("DeleteLine only: got %q, want %q", got, want)
	}
}

func TestDeleteLine_Selection(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc\nddd")
	e.SelectionActive = true
	e.SelectStartLine = 1
	e.SelectStartCol = 0
	e.CursorLine = 2
	e.CursorCol = 2
	e.DeleteLine()
	want := "aaa\nddd"
	if got := bufText(e); got != want {
		t.Errorf("DeleteLine selection: got %q, want %q", got, want)
	}
}

func TestDeleteLine_Undo(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.DeleteLine()
	e.Undo()
	want := "aaa\nbbb\nccc"
	if got := bufText(e); got != want {
		t.Errorf("DeleteLine undo: got %q, want %q", got, want)
	}
}

func TestDeleteLine_AfterSelectLine(t *testing.T) {
	// SelectLine places cursor at (nextLine, 0) — half-open selection.
	// DeleteLine must only delete the selected line, not the next one.
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.SelectLine() // selects "bbb", cursor now at (2, 0)
	e.DeleteLine()
	want := "aaa\nccc"
	if got := bufText(e); got != want {
		t.Errorf("DeleteLine after SelectLine: got %q, want %q", got, want)
	}
}

// --- SwapLineUp / SwapLineDown ---

func TestSwapLineUp(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.SwapLineUp()
	want := "bbb\naaa\nccc"
	if got := bufText(e); got != want {
		t.Errorf("SwapLineUp: got %q, want %q", got, want)
	}
	if e.CursorLine != 0 {
		t.Errorf("cursor: got %d, want 0", e.CursorLine)
	}
}

func TestSwapLineUp_FirstLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb")
	e.CursorLine = 0
	e.SwapLineUp()
	want := "aaa\nbbb"
	if got := bufText(e); got != want {
		t.Errorf("SwapLineUp first: got %q, want %q", got, want)
	}
}

func TestSwapLineDown(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.SwapLineDown()
	want := "aaa\nccc\nbbb"
	if got := bufText(e); got != want {
		t.Errorf("SwapLineDown: got %q, want %q", got, want)
	}
	if e.CursorLine != 2 {
		t.Errorf("cursor: got %d, want 2", e.CursorLine)
	}
}

func TestSwapLineDown_LastLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb")
	e.CursorLine = 1
	e.SwapLineDown()
	want := "aaa\nbbb"
	if got := bufText(e); got != want {
		t.Errorf("SwapLineDown last: got %q, want %q", got, want)
	}
}

func TestSwapLine_Undo(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.SwapLineUp()
	e.Undo()
	want := "aaa\nbbb\nccc"
	if got := bufText(e); got != want {
		t.Errorf("SwapLine undo: got %q, want %q", got, want)
	}
}

// --- DuplicateLine ---

func TestDuplicateLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.DuplicateLine()
	want := "aaa\nbbb\nbbb\nccc"
	if got := bufText(e); got != want {
		t.Errorf("DuplicateLine: got %q, want %q", got, want)
	}
	if e.CursorLine != 2 {
		t.Errorf("cursor: got %d, want 2", e.CursorLine)
	}
}

func TestDuplicateLine_Undo(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb")
	e.CursorLine = 0
	e.DuplicateLine()
	e.Undo()
	want := "aaa\nbbb"
	if got := bufText(e); got != want {
		t.Errorf("DuplicateLine undo: got %q, want %q", got, want)
	}
}

// --- ToggleLineComment ---

func TestToggleLineComment_AddComment(t *testing.T) {
	e := newLineOpsEditor("fmt.Println()")
	e.ToggleLineComment("//")
	want := "// fmt.Println()"
	if got := bufText(e); got != want {
		t.Errorf("AddComment: got %q, want %q", got, want)
	}
}

func TestToggleLineComment_RemoveComment(t *testing.T) {
	e := newLineOpsEditor("// fmt.Println()")
	e.ToggleLineComment("//")
	want := "fmt.Println()"
	if got := bufText(e); got != want {
		t.Errorf("RemoveComment: got %q, want %q", got, want)
	}
}

func TestToggleLineComment_IndentedCode(t *testing.T) {
	e := newLineOpsEditor("\treturn nil")
	e.ToggleLineComment("//")
	want := "\t// return nil"
	if got := bufText(e); got != want {
		t.Errorf("IndentedComment: got %q, want %q", got, want)
	}
}

func TestToggleLineComment_MixedSelection(t *testing.T) {
	e := newLineOpsEditor("commented\n// already\nplain")
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 0
	e.CursorLine = 2
	e.CursorCol = 5
	e.ToggleLineComment("//")
	// Not all lines are commented, so all should get comment prefix.
	want := "// commented\n// // already\n// plain"
	if got := bufText(e); got != want {
		t.Errorf("MixedComment: got %q, want %q", got, want)
	}
}

func TestToggleLineComment_Undo(t *testing.T) {
	e := newLineOpsEditor("code")
	e.ToggleLineComment("//")
	e.Undo()
	want := "code"
	if got := bufText(e); got != want {
		t.Errorf("ToggleComment undo: got %q, want %q", got, want)
	}
}

// --- IndentSelection / OutdentSelection ---

func TestIndentSelection(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 0
	e.CursorLine = 2
	e.CursorCol = 3
	e.IndentSelection("    ")
	want := "    aaa\n    bbb\n    ccc"
	if got := bufText(e); got != want {
		t.Errorf("IndentSelection: got %q, want %q", got, want)
	}
}

func TestOutdentSelection(t *testing.T) {
	e := newLineOpsEditor("    aaa\n    bbb\n    ccc")
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 0
	e.CursorLine = 2
	e.CursorCol = 7
	e.OutdentSelection("    ")
	want := "aaa\nbbb\nccc"
	if got := bufText(e); got != want {
		t.Errorf("OutdentSelection: got %q, want %q", got, want)
	}
}

func TestOutdentSelection_PartialSpaces(t *testing.T) {
	e := newLineOpsEditor("  aaa\n  bbb")
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 4 // cursor at col 4 ("a")
	e.CursorLine = 1
	e.CursorCol = 5 // cursor at col 5 ("b")
	e.OutdentSelection("    ")
	want := "aaa\nbbb"
	if got := bufText(e); got != want {
		t.Errorf("OutdentSelection partial: got %q, want %q", got, want)
	}
	// Only 2 spaces were removed per line, so columns should shift by 2, not 4.
	if e.SelectStartCol != 2 {
		t.Errorf("anchor col: got %d, want 2", e.SelectStartCol)
	}
	if e.CursorCol != 3 {
		t.Errorf("cursor col: got %d, want 3", e.CursorCol)
	}
}

// --- SelectLine ---

func TestSelectLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 1
	e.SelectLine()
	if !e.SelectionActive {
		t.Fatal("SelectLine: selection not active")
	}
	if e.SelectStartLine != 1 || e.SelectStartCol != 0 {
		t.Errorf("SelectLine start: got (%d,%d)", e.SelectStartLine, e.SelectStartCol)
	}
	if e.CursorLine != 2 || e.CursorCol != 0 {
		t.Errorf("SelectLine cursor: got (%d,%d)", e.CursorLine, e.CursorCol)
	}
}

func TestSelectLine_Extend(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb\nccc")
	e.CursorLine = 0
	e.SelectLine()
	e.SelectLine() // extend
	if e.CursorLine != 2 || e.CursorCol != 0 {
		t.Errorf("SelectLine extend: got (%d,%d), want (2,0)", e.CursorLine, e.CursorCol)
	}
}

func TestSelectLine_LastLine(t *testing.T) {
	e := newLineOpsEditor("aaa\nbbb")
	e.CursorLine = 1
	e.SelectLine()
	if e.CursorLine != 1 || e.CursorCol != 3 {
		t.Errorf("SelectLine last: got (%d,%d), want (1,3)", e.CursorLine, e.CursorCol)
	}
}

// --- SelectNextOccurrence ---

func TestSelectNextOccurrence_SelectWord(t *testing.T) {
	e := newLineOpsEditor("hello world hello")
	e.CursorCol = 2 // middle of "hello"
	e.SelectNextOccurrence()
	if !e.SelectionActive {
		t.Fatal("SelectNextOccurrence: selection not active")
	}
	got := e.SelectedText()
	if got != "hello" {
		t.Errorf("SelectNextOccurrence word: got %q, want %q", got, "hello")
	}
}

func TestSelectNextOccurrence_FindNext(t *testing.T) {
	e := newLineOpsEditor("hello world hello")
	// Select first "hello"
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 0
	e.CursorLine = 0
	e.CursorCol = 5
	e.SelectNextOccurrence()
	// Should jump to second "hello"
	if e.SelectStartCol != 12 || e.CursorCol != 17 {
		t.Errorf("FindNext: got selection (%d,%d), want (12,17)", e.SelectStartCol, e.CursorCol)
	}
}

func TestSelectNextOccurrence_Wrap(t *testing.T) {
	e := newLineOpsEditor("hello world hello")
	// Select second "hello"
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 12
	e.CursorLine = 0
	e.CursorCol = 17
	e.SelectNextOccurrence()
	// Should wrap to first "hello"
	if e.SelectStartCol != 0 || e.CursorCol != 5 {
		t.Errorf("Wrap: got selection (%d,%d), want (0,5)", e.SelectStartCol, e.CursorCol)
	}
}
