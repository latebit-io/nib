package textarea

import "testing"

func TestWrap_SingleLineFits(t *testing.T) {
	ta := New(20)
	ta.SetContent("short")

	lines := ta.Render()
	if len(lines) != 1 {
		t.Fatalf("got %d visual lines, want 1", len(lines))
	}
	if lines[0].Text != "short" {
		t.Errorf("line = %q, want %q", lines[0].Text, "short")
	}
}

func TestWrap_LineWraps(t *testing.T) {
	ta := New(10)
	ta.SetContent("hello world foo")

	lines := ta.Render()
	if len(lines) < 2 {
		t.Fatalf("got %d visual lines, want >= 2", len(lines))
	}
}

func TestWrap_EmptyLine(t *testing.T) {
	ta := New(20)
	ta.SetContent("")

	lines := ta.Render()
	if len(lines) != 1 {
		t.Fatalf("got %d visual lines, want 1", len(lines))
	}
}

func TestWrap_ExactWidth(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcde")

	lines := ta.Render()
	if len(lines) != 1 {
		t.Fatalf("got %d visual lines, want 1 (exact fit)", len(lines))
	}
}

func TestWrap_MultipleLogicalLines(t *testing.T) {
	ta := New(20)
	ta.SetContent("line one\nline two\nline three")

	lines := ta.Render()
	if len(lines) != 3 {
		t.Fatalf("got %d visual lines, want 3", len(lines))
	}
}

func TestWrap_LogicalToVisual(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcdefgh") // wraps to "abcde" + "fgh"

	// Cursor at logical col 6 (on 'g') should be visual row 1, col 1.
	vr, vc := ta.logicalToVisual(0, 6)
	if vr != 1 || vc != 1 {
		t.Errorf("logicalToVisual(0,6) = (%d,%d), want (1,1)", vr, vc)
	}
}

func TestWrap_VisualToLogical(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcdefgh") // wraps to "abcde" + "fgh"

	// Visual row 1, col 1 should be logical (0, 6).
	ll, lc := ta.visualToLogical(1, 1)
	if ll != 0 || lc != 6 {
		t.Errorf("visualToLogical(1,1) = (%d,%d), want (0,6)", ll, lc)
	}
}

func TestWrap_CursorPositionAfterWrap(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcdefgh")
	// SetContent puts cursor at end: logical (0, 8)
	// Visual should be row 1, col 3.

	vr, vc := ta.CursorPosition()
	if vr != 1 || vc != 3 {
		t.Errorf("CursorPosition() = (%d,%d), want (1,3)", vr, vc)
	}
}

func TestWrap_UpDownAcrossVisualLines(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcdefgh") // wraps: "abcde" + "fgh"
	ta.cursorLine = 0
	ta.cursorCol = 2 // logical col 2 → visual (0, 2)

	ta.moveDown()
	// Should move to visual row 1, col 2 → logical (0, 7)
	if ta.cursorCol != 7 {
		t.Errorf("after down: col = %d, want 7", ta.cursorCol)
	}

	ta.moveUp()
	if ta.cursorCol != 2 {
		t.Errorf("after up: col = %d, want 2", ta.cursorCol)
	}
}

func TestWrap_DownClampedToShorterLine(t *testing.T) {
	ta := New(10)
	ta.SetContent("longline!!\nhi")
	ta.cursorLine = 0
	ta.cursorCol = 9 // end of first line

	ta.moveDown()
	// Second line "hi" has only 2 chars, cursor should clamp to 2.
	if ta.cursorLine != 1 || ta.cursorCol != 2 {
		t.Errorf("cursor = (%d,%d), want (1,2)", ta.cursorLine, ta.cursorCol)
	}
}

func TestWrap_VisualLineCount(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcdefgh\nxy")

	// "abcdefgh" wraps to 2 lines, "xy" is 1 line → 3 total.
	if got := ta.VisualLineCount(); got != 3 {
		t.Errorf("VisualLineCount() = %d, want 3", got)
	}
}

func TestWrap_InvalidatesOnSetSize(t *testing.T) {
	ta := New(5)
	ta.SetContent("abcdefgh") // wraps to 2 visual lines

	if ta.VisualLineCount() != 2 {
		t.Fatalf("before resize: %d visual lines, want 2", ta.VisualLineCount())
	}

	ta.SetSize(20)
	if ta.VisualLineCount() != 1 {
		t.Errorf("after resize to 20: %d visual lines, want 1", ta.VisualLineCount())
	}
}
