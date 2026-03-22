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

	// Replace "world" (starts at col 6, 5 runes) with "earth", 1 char/tick
	ie := e.BeginIncrementalEdit(0, 6, 5, 1, "earth")

	if got := e.Buf.Content(); got != "hello " {
		t.Fatalf("after delete: %q, want %q", got, "hello ")
	}

	// Advance 5 ticks (1 char each)
	for range 5 {
		result := ie.Advance()
		if result.Done && ie.Remaining() > 0 {
			t.Fatal("reported done with remaining chars")
		}
	}

	if got := e.Buf.Content(); got != "hello earth" {
		t.Errorf("after insert: %q, want %q", got, "hello earth")
	}

	line, col := ie.Position()
	if line != 0 || col != 11 {
		t.Errorf("position = %d:%d, want 0:11", line, col)
	}

	ie.Complete()

	ok := e.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if got := e.Buf.Content(); got != "hello world" {
		t.Errorf("after undo: %q, want %q", got, "hello world")
	}
}

func TestIncrementalEdit_MultiCharPerTick(t *testing.T) {
	e := newTestEditor("old")

	// Replace "old" with "new text", 3 chars/tick
	ie := e.BeginIncrementalEdit(0, 0, 3, 3, "new text")

	// First tick: inserts "new"
	r1 := ie.Advance()
	if r1.Done {
		t.Fatal("should not be done after first tick")
	}
	if got := e.Buf.Content(); got != "new" {
		t.Errorf("after tick 1: %q, want %q", got, "new")
	}

	// Second tick: inserts " te"
	ie.Advance()
	if got := e.Buf.Content(); got != "new te" {
		t.Errorf("after tick 2: %q, want %q", got, "new te")
	}

	// Third tick: inserts "xt" and reports done
	r3 := ie.Advance()
	if !r3.Done {
		t.Error("should be done after all chars inserted")
	}
	if got := e.Buf.Content(); got != "new text" {
		t.Errorf("after tick 3: %q, want %q", got, "new text")
	}

	ie.Complete()
}

func TestIncrementalEdit_NewlinePause(t *testing.T) {
	e := newTestEditor("x")

	// Replace "x" with "a\nb", 3 chars/tick — newline should stop the tick early
	ie := e.BeginIncrementalEdit(0, 0, 1, 3, "a\nb")

	// First tick: inserts "a\n" then stops (newline pause)
	r1 := ie.Advance()
	if r1.Done {
		t.Fatal("should not be done")
	}
	if !r1.HitNewline {
		t.Error("expected HitNewline on first tick")
	}
	if got := e.Buf.Content(); got != "a\n" {
		t.Errorf("after tick 1: %q, want %q", got, "a\n")
	}

	// Second tick: inserts "b"
	r2 := ie.Advance()
	if !r2.Done {
		t.Error("should be done after all chars")
	}

	ie.Complete()
}

func TestIncrementalEdit_MultilineInsert(t *testing.T) {
	e := newTestEditor("func main() {}")

	ie := e.BeginIncrementalEdit(0, 12, 2, 100, "{\n\treturn\n}")

	// Single tick should insert everything (100 chars/tick > 11 runes)
	// But newlines cause early stops, so multiple ticks needed.
	for !ie.IsComplete() {
		result := ie.Advance()
		if result.Done {
			break
		}
	}

	want := "func main() {\n\treturn\n}"
	if got := e.Buf.Content(); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	startLine, startCol := ie.StartPosition()
	if startLine != 0 || startCol != 12 {
		t.Errorf("start position = %d:%d, want 0:12", startLine, startCol)
	}

	ie.Complete()
	e.Undo()
	if got := e.Buf.Content(); got != "func main() {}" {
		t.Errorf("after undo: %q, want %q", got, "func main() {}")
	}
}

func TestIncrementalEdit_Abort(t *testing.T) {
	e := newTestEditor("abcdef")

	ie := e.BeginIncrementalEdit(0, 2, 2, 1, "XYZ")
	ie.Advance() // inserts "X"
	ie.Abort()

	if got := e.Buf.Content(); got != "abXef" {
		t.Errorf("after abort: %q, want %q", got, "abXef")
	}

	e.Undo()
	if got := e.Buf.Content(); got != "abcdef" {
		t.Errorf("after undo: %q, want %q", got, "abcdef")
	}
}

func TestIncrementalEdit_DoubleComplete(t *testing.T) {
	e := newTestEditor("test")
	ie := e.BeginIncrementalEdit(0, 0, 4, 100, "done")
	ie.Advance()
	ie.Complete()
	ie.Complete() // second call should be safe

	if !ie.IsComplete() {
		t.Error("expected IsComplete to be true")
	}
}

func TestIncrementalEdit_EmptyReplace(t *testing.T) {
	e := newTestEditor("hello")

	// Pure deletion — 0 replacement chars
	ie := e.BeginIncrementalEdit(0, 3, 2, 1, "")

	if ie.Remaining() != 0 {
		t.Errorf("remaining = %d, want 0", ie.Remaining())
	}

	result := ie.Advance()
	if !result.Done {
		t.Error("empty replace should be done immediately")
	}

	ie.Complete()
	if got := e.Buf.Content(); got != "hel" {
		t.Errorf("content = %q, want %q", got, "hel")
	}

	e.Undo()
	if got := e.Buf.Content(); got != "hello" {
		t.Errorf("after undo: %q, want %q", got, "hello")
	}
}

func TestIncrementalEdit_FinishLine(t *testing.T) {
	e := newTestEditor("old")

	ie := e.BeginIncrementalEdit(0, 0, 3, 1, "abc\ndef")
	ie.Advance() // inserts "a"

	// FinishLine should insert "bc" and stop before newline.
	ie.FinishLine()

	if got := e.Buf.Content(); got != "abc" {
		t.Errorf("after FinishLine: %q, want %q", got, "abc")
	}

	// 3 chars consumed (a, b, c), 4 remaining (\n, d, e, f)
	if ie.Remaining() != 4 {
		t.Errorf("remaining = %d, want 4", ie.Remaining())
	}

	ie.Abort()
}

func TestIncrementalEdit_AdvanceAfterComplete(t *testing.T) {
	e := newTestEditor("test")
	ie := e.BeginIncrementalEdit(0, 0, 4, 1, "new")
	ie.Complete()

	// Advance after Complete should report done.
	result := ie.Advance()
	if !result.Done {
		t.Error("advance after complete should be done")
	}
}
