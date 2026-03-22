package editor

import (
	"testing"

	"github.com/latebit-io/junto/engine/buffer"
)

func newTestEditor(content string) *Editor {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	return New(buf)
}

func TestIncrementalEdit_BasicFlow(t *testing.T) {
	e := newTestEditor("hello world")

	// Replace "world" (starts at col 6, 5 runes) with "earth"
	ie := e.BeginIncrementalEdit(0, 6, 5)

	// After delete, buffer should be "hello "
	if got := e.Buf.Content(); got != "hello " {
		t.Fatalf("after delete: %q, want %q", got, "hello ")
	}

	for _, r := range "earth" {
		ie.InsertChar(r)
	}

	if got := e.Buf.Content(); got != "hello earth" {
		t.Errorf("after insert: %q, want %q", got, "hello earth")
	}

	line, col := ie.Position()
	if line != 0 || col != 11 {
		t.Errorf("position = %d:%d, want 0:11", line, col)
	}

	ie.Complete()

	// Undo should reverse the entire edit atomically.
	ok := e.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if got := e.Buf.Content(); got != "hello world" {
		t.Errorf("after undo: %q, want %q", got, "hello world")
	}
}

func TestIncrementalEdit_MultilineInsert(t *testing.T) {
	e := newTestEditor("func main() {}")

	// Replace "{}" (starts at col 12, 2 runes) with "{\n\treturn\n}"
	ie := e.BeginIncrementalEdit(0, 12, 2)

	for _, r := range "{\n\treturn\n}" {
		ie.InsertChar(r)
	}

	line, col := ie.Position()
	if line != 2 || col != 1 {
		t.Errorf("position = %d:%d, want 2:1", line, col)
	}

	startLine, startCol := ie.StartPosition()
	if startLine != 0 || startCol != 12 {
		t.Errorf("start position = %d:%d, want 0:12", startLine, startCol)
	}

	ie.Complete()

	want := "func main() {\n\treturn\n}"
	if got := e.Buf.Content(); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	// Undo reverses all three lines back to original.
	e.Undo()
	if got := e.Buf.Content(); got != "func main() {}" {
		t.Errorf("after undo: %q, want %q", got, "func main() {}")
	}
}

func TestIncrementalEdit_Abort(t *testing.T) {
	e := newTestEditor("abcdef")

	// Delete "cd" (col 2, 2 runes) and partially type "XY"
	ie := e.BeginIncrementalEdit(0, 2, 2)
	ie.InsertChar('X')
	// Abort before finishing
	ie.Abort()

	if got := e.Buf.Content(); got != "abXef" {
		t.Errorf("after abort: %q, want %q", got, "abXef")
	}

	// Undo should reverse the entire partial edit.
	e.Undo()
	if got := e.Buf.Content(); got != "abcdef" {
		t.Errorf("after undo: %q, want %q", got, "abcdef")
	}
}

func TestIncrementalEdit_DoubleComplete(t *testing.T) {
	e := newTestEditor("test")
	ie := e.BeginIncrementalEdit(0, 0, 4)
	for _, r := range "done" {
		ie.InsertChar(r)
	}
	ie.Complete()
	// Second Complete should be safe (no-op).
	ie.Complete()

	if !ie.IsComplete() {
		t.Error("expected IsComplete to be true")
	}
}

func TestIncrementalEdit_InsertAfterComplete(t *testing.T) {
	e := newTestEditor("test")
	ie := e.BeginIncrementalEdit(0, 0, 4)
	ie.Complete()

	// InsertChar after Complete should be a no-op.
	ie.InsertChar('X')
	if got := e.Buf.Content(); got != "" {
		t.Errorf("content = %q, want %q", got, "")
	}
}

func TestIncrementalEdit_FinishLine(t *testing.T) {
	e := newTestEditor("old")

	// Delete "old" and start typing "abc\ndef"
	ie := e.BeginIncrementalEdit(0, 0, 3)
	ie.InsertChar('a')

	// FinishLine with remaining "bc\ndef" — should consume "bc" and stop.
	remaining := []rune("bc\ndef")
	consumed := ie.FinishLine(remaining)

	if consumed != 2 {
		t.Errorf("consumed = %d, want 2", consumed)
	}

	line, col := ie.Position()
	if line != 0 || col != 3 {
		t.Errorf("position = %d:%d, want 0:3", line, col)
	}

	if got := e.Buf.Content(); got != "abc" {
		t.Errorf("content = %q, want %q", got, "abc")
	}

	ie.Abort()
}

func TestIncrementalEdit_EmptyDelete(t *testing.T) {
	e := newTestEditor("hello")

	// Delete 0 runes (pure insertion)
	ie := e.BeginIncrementalEdit(0, 5, 0)
	ie.InsertChar('!')
	ie.Complete()

	if got := e.Buf.Content(); got != "hello!" {
		t.Errorf("content = %q, want %q", got, "hello!")
	}

	e.Undo()
	if got := e.Buf.Content(); got != "hello" {
		t.Errorf("after undo: %q, want %q", got, "hello")
	}
}
