package ui

import (
	"testing"

	"github.com/latebit-io/nib/engine/openfile"
)

func TestNewDiffOverlay(t *testing.T) {
	diff := &openfile.DiffResult{
		StartLine: 2,
		EndLine:   4,
		NewLines:  []string{"line A", "line B"},
	}
	o := NewDiffOverlay(diff)
	if o.StartLine != 2 {
		t.Errorf("StartLine = %d, want 2", o.StartLine)
	}
	if o.EndLine != 4 {
		t.Errorf("EndLine = %d, want 4", o.EndLine)
	}
	if o.LineCount() != 2 {
		t.Errorf("LineCount() = %d, want 2", o.LineCount())
	}
	if o.LineText(0) != "line A" {
		t.Errorf("LineText(0) = %q, want %q", o.LineText(0), "line A")
	}
	if o.Active {
		t.Error("expected Active = false on creation")
	}
}

func TestOverlayEditing(t *testing.T) {
	o := newTestOverlay("hello")
	oe := o.Editor

	// Insert at end
	oe.MoveCursorTo(0, 5)
	oe.InsertChar('!')
	if o.Content() != "hello!" {
		t.Errorf("Content() = %q, want %q", o.Content(), "hello!")
	}

	// Insert in middle
	o = newTestOverlay("hllo")
	oe = o.Editor
	oe.MoveCursorTo(0, 1)
	oe.InsertChar('e')
	if o.Content() != "hello" {
		t.Errorf("Content() = %q, want %q", o.Content(), "hello")
	}
}

func TestOverlayNewline(t *testing.T) {
	o := newTestOverlay("hello world")
	oe := o.Editor
	oe.MoveCursorTo(0, 5)
	oe.InsertNewline()
	if o.LineCount() != 2 {
		t.Fatalf("LineCount() = %d, want 2", o.LineCount())
	}
	if o.LineText(0) != "hello" {
		t.Errorf("line 0 = %q, want %q", o.LineText(0), "hello")
	}
	if o.LineText(1) != " world" {
		t.Errorf("line 1 = %q, want %q", o.LineText(1), " world")
	}
}

func TestOverlayBackspace(t *testing.T) {
	tests := []struct {
		name    string
		lines   []string
		line    int
		col     int
		wantStr string
	}{
		{
			name:    "delete char",
			lines:   []string{"hello"},
			line:    0,
			col:     3,
			wantStr: "helo",
		},
		{
			name:    "join lines",
			lines:   []string{"hello", "world"},
			line:    1,
			col:     0,
			wantStr: "helloworld",
		},
		{
			name:    "noop at start",
			lines:   []string{"hello"},
			line:    0,
			col:     0,
			wantStr: "hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newTestOverlayMulti(tt.lines)
			o.Editor.MoveCursorTo(tt.line, tt.col)
			o.Editor.Backspace()
			if o.Content() != tt.wantStr {
				t.Errorf("Content() = %q, want %q", o.Content(), tt.wantStr)
			}
		})
	}
}

func TestOverlaySelection(t *testing.T) {
	o := newTestOverlayMulti([]string{"hello", "world"})
	oe := o.Editor

	// Select "llo"
	oe.MoveCursorTo(0, 2)
	oe.StartSelection()
	oe.MoveCursorTo(0, 5)
	if !oe.SelectionActive {
		t.Fatal("expected selection active")
	}
	if oe.SelectedText() != "llo" {
		t.Errorf("SelectedText() = %q, want %q", oe.SelectedText(), "llo")
	}

	// Delete selection
	oe.DeleteSelection()
	if o.Content() != "he\nworld" {
		t.Errorf("after delete: Content() = %q, want %q", o.Content(), "he\nworld")
	}
}

func TestOverlayContent(t *testing.T) {
	o := newTestOverlayMulti([]string{"a", "b", "c"})
	if o.Content() != "a\nb\nc" {
		t.Errorf("Content() = %q, want %q", o.Content(), "a\nb\nc")
	}
}

func TestOverlayLineBounds(t *testing.T) {
	o := newTestOverlay("hello")
	if o.LineText(-1) != "" {
		t.Error("LineText(-1) should be empty")
	}
	if o.LineText(5) != "" {
		t.Error("LineText(5) should be empty")
	}
}

func TestOverlayPaste(t *testing.T) {
	o := newTestOverlay("ac")
	o.Editor.MoveCursorTo(0, 1)
	o.Editor.PasteText("b\nd")
	if o.LineCount() != 2 {
		t.Fatalf("LineCount() = %d, want 2", o.LineCount())
	}
	if o.LineText(0) != "ab" {
		t.Errorf("line 0 = %q, want %q", o.LineText(0), "ab")
	}
	if o.LineText(1) != "dc" {
		t.Errorf("line 1 = %q, want %q", o.LineText(1), "dc")
	}
}

// --- Helpers ---

func newTestOverlay(text string) *DiffOverlay {
	return NewDiffOverlay(&openfile.DiffResult{
		StartLine: 0,
		EndLine:   0,
		NewLines:  []string{text},
	})
}

func newTestOverlayMulti(lines []string) *DiffOverlay {
	return NewDiffOverlay(&openfile.DiffResult{
		StartLine: 0,
		EndLine:   len(lines) - 1,
		NewLines:  lines,
	})
}
