package editor

import (
	"testing"

	"github.com/latebit-io/junto/engine/buffer"
)

func newScrollEditor(content string, width, height int) *Editor {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	e := New(buf)
	e.SetSize(width, height)
	return e
}

func TestBufferColToDisplayCol(t *testing.T) {
	tests := []struct {
		name    string
		content string
		line    int
		bufCol  int
		want    int
	}{
		{"no tabs", "hello world", 0, 5, 5},
		{"single tab at start", "\thello", 0, 1, 4},
		{"tab in middle", "ab\tcd", 0, 3, 6}, // a=0, b=1, \t=2-5, c=6
		{"multiple tabs", "\t\tx", 0, 2, 8},
		{"VS16 has zero display width", "✏\uFE0Fx", 0, 2, 1},
		{"col past line end", "abc", 0, 10, 3},
		{"col zero", "anything", 0, 0, 0},
		{"empty line", "", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newScrollEditor(tt.content, 80, 24)
			got := e.BufferColToDisplayCol(tt.line, tt.bufCol)
			if got != tt.want {
				t.Errorf("BufferColToDisplayCol(%d, %d) = %d, want %d",
					tt.line, tt.bufCol, got, tt.want)
			}
		})
	}
}

func TestEnsureCursorVisible_ScrollsRight(t *testing.T) {
	// 20-col content area (gutter=4 for 1-line file → contentW=16 actually)
	// Use a wider editor for predictable math.
	e := newScrollEditor("abcdefghijklmnopqrstuvwxyz0123456789", 30, 10)
	cw := e.ContentWidth() // 30 - 4 = 26

	// Move cursor to end of line (col 36 → display col 36).
	e.MoveCursorTo(0, 36)
	// Cursor at display col 36 should scroll right.
	if e.ScrollCol <= 0 {
		t.Errorf("expected ScrollCol > 0 after moving cursor to col 36, got %d", e.ScrollCol)
	}
	// Cursor should be visible: displayCursorCol - ScrollCol < contentWidth.
	dispCol := e.BufferColToDisplayCol(0, e.CursorCol)
	if dispCol < e.ScrollCol || dispCol >= e.ScrollCol+cw {
		t.Errorf("cursor at display col %d not visible with ScrollCol=%d contentW=%d",
			dispCol, e.ScrollCol, cw)
	}
}

func TestEnsureCursorVisible_ScrollsLeft(t *testing.T) {
	e := newScrollEditor("abcdefghijklmnopqrstuvwxyz0123456789", 30, 10)

	// Scroll right first.
	e.MoveCursorTo(0, 36)
	if e.ScrollCol <= 0 {
		t.Fatal("precondition: expected ScrollCol > 0")
	}

	// Move cursor back to column 0.
	e.MoveCursorTo(0, 0)
	if e.ScrollCol != 0 {
		t.Errorf("expected ScrollCol=0 after cursor at col 0, got %d", e.ScrollCol)
	}
}

func TestEnsureCursorVisible_TabsAccountedFor(t *testing.T) {
	// Line with tabs: each tab = 4 display cols.
	e := newScrollEditor("\t\t\t\t\t\t\t\t\tend", 30, 10) // 9 tabs + "end" = 39 display cols
	cw := e.ContentWidth()

	e.MoveCursorTo(0, 12) // past end of "end" (buffer col 12 = display col 39)
	dispCol := e.BufferColToDisplayCol(0, e.CursorCol)
	if dispCol < e.ScrollCol || dispCol >= e.ScrollCol+cw {
		t.Errorf("cursor at display col %d not visible with ScrollCol=%d cw=%d",
			dispCol, e.ScrollCol, cw)
	}
}

func TestClampScrollCol(t *testing.T) {
	e := newScrollEditor("hello", 80, 24)
	e.ScrollCol = -5
	e.ClampScrollCol()
	if e.ScrollCol != 0 {
		t.Errorf("expected ScrollCol=0 after clamp, got %d", e.ScrollCol)
	}
}

func TestScrollLeftRight(t *testing.T) {
	e := newScrollEditor("hello", 80, 24)
	e.ScrollRight(10)
	if e.ScrollCol != 10 {
		t.Errorf("expected ScrollCol=10, got %d", e.ScrollCol)
	}
	e.ScrollLeft(3)
	if e.ScrollCol != 7 {
		t.Errorf("expected ScrollCol=7, got %d", e.ScrollCol)
	}
	e.ScrollLeft(100)
	if e.ScrollCol != 0 {
		t.Errorf("expected ScrollCol=0 after large left scroll, got %d", e.ScrollCol)
	}
}

func TestSetSize_ClampsScrollCol(t *testing.T) {
	e := newScrollEditor("hello", 80, 24)
	e.ScrollCol = -10
	e.SetSize(40, 12)
	if e.ScrollCol != 0 {
		t.Errorf("expected ScrollCol=0 after SetSize clamp, got %d", e.ScrollCol)
	}
}
