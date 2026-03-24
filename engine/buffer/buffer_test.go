package buffer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewEmpty(t *testing.T) {
	b := New()
	if b.LineCount() != 1 {
		t.Errorf("expected 1 line, got %d", b.LineCount())
	}
	if b.Content() != "" {
		t.Errorf("expected empty content, got %q", b.Content())
	}
}

func TestNewFromString(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantLines int
		wantLine0 string
	}{
		{"single line", "hello", 1, "hello"},
		{"two lines", "hello\nworld", 2, "hello"},
		{"trailing newline", "hello\n", 1, "hello"},
		{"empty lines", "a\n\nb", 3, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewFromString(tt.input)
			if b.LineCount() != tt.wantLines {
				t.Errorf("LineCount: got %d, want %d", b.LineCount(), tt.wantLines)
			}
			if b.LineText(0) != tt.wantLine0 {
				t.Errorf("Line(0): got %q, want %q", b.LineText(0), tt.wantLine0)
			}
		})
	}
}

func TestNewFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.LineCount() != 2 {
		t.Errorf("expected 2 lines, got %d", b.LineCount())
	}
	if b.Path != path {
		t.Errorf("expected path %q, got %q", path, b.Path)
	}
}

func TestInsertSingleLine(t *testing.T) {
	b := NewFromString("hello world")
	b.Insert(0, 5, " beautiful")
	if b.LineText(0) != "hello beautiful world" {
		t.Errorf("got %q", b.LineText(0))
	}
	if !b.Modified {
		t.Error("expected Modified=true")
	}
}

func TestInsertNewline(t *testing.T) {
	b := NewFromString("hello world")
	b.Insert(0, 5, "\n")
	if b.LineCount() != 2 {
		t.Errorf("expected 2 lines, got %d", b.LineCount())
	}
	if b.LineText(0) != "hello" {
		t.Errorf("line 0: got %q", b.LineText(0))
	}
	if b.LineText(1) != " world" {
		t.Errorf("line 1: got %q", b.LineText(1))
	}
}

func TestInsertMultiLine(t *testing.T) {
	b := NewFromString("ab")
	b.Insert(0, 1, "X\nY\nZ")
	// Should be: "aX", "Y", "Zb"
	if b.LineCount() != 3 {
		t.Fatalf("expected 3 lines, got %d", b.LineCount())
	}
	if b.LineText(0) != "aX" {
		t.Errorf("line 0: got %q", b.LineText(0))
	}
	if b.LineText(1) != "Y" {
		t.Errorf("line 1: got %q", b.LineText(1))
	}
	if b.LineText(2) != "Zb" {
		t.Errorf("line 2: got %q", b.LineText(2))
	}
}

func TestDeleteSingleLine(t *testing.T) {
	b := NewFromString("hello world")
	deleted := b.Delete(0, 5, 6)
	if deleted != " world" {
		t.Errorf("deleted: got %q, want %q", deleted, " world")
	}
	if b.LineText(0) != "hello" {
		t.Errorf("got %q", b.LineText(0))
	}
}

func TestDeleteAcrossNewline(t *testing.T) {
	b := NewFromString("hello\nworld")
	// Delete from col 3 of line 0, across the newline, into line 1
	deleted := b.Delete(0, 3, 4) // "lo\nw"
	if deleted != "lo\nw" {
		t.Errorf("deleted: got %q, want %q", deleted, "lo\nw")
	}
	if b.LineCount() != 1 {
		t.Errorf("expected 1 line, got %d", b.LineCount())
	}
	if b.LineText(0) != "helorld" {
		t.Errorf("got %q", b.LineText(0))
	}
}

func TestUndoRedo(t *testing.T) {
	b := NewFromString("hello")

	b.Insert(0, 5, " world")
	if b.LineText(0) != "hello world" {
		t.Fatalf("after insert: got %q", b.LineText(0))
	}

	l, c, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if l != 0 || c != 5 {
		t.Errorf("undo cursor: got (%d,%d), want (0,5)", l, c)
	}
	if b.LineText(0) != "hello" {
		t.Errorf("after undo: got %q", b.LineText(0))
	}

	l, c, ok = b.Redo()
	if !ok {
		t.Fatal("redo failed")
	}
	if l != 0 || c != 11 {
		t.Errorf("redo cursor: got (%d,%d), want (0,11)", l, c)
	}
	if b.LineText(0) != "hello world" {
		t.Errorf("after redo: got %q", b.LineText(0))
	}
}

func TestUndoDelete(t *testing.T) {
	b := NewFromString("hello world")
	b.Delete(0, 5, 6)
	if b.LineText(0) != "hello" {
		t.Fatalf("after delete: got %q", b.LineText(0))
	}

	_, _, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if b.LineText(0) != "hello world" {
		t.Errorf("after undo: got %q", b.LineText(0))
	}
}

func TestGroupUndoRedo(t *testing.T) {
	b := NewFromString("hello")

	b.BeginGroup()
	b.Insert(0, 5, " beautiful")
	b.Insert(0, 15, " world")
	b.EndGroup()

	if b.LineText(0) != "hello beautiful world" {
		t.Fatalf("after group: got %q", b.LineText(0))
	}

	_, _, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if b.LineText(0) != "hello" {
		t.Errorf("after group undo: got %q", b.LineText(0))
	}

	_, _, ok = b.Redo()
	if !ok {
		t.Fatal("redo failed")
	}
	if b.LineText(0) != "hello beautiful world" {
		t.Errorf("after group redo: got %q", b.LineText(0))
	}
}

func TestSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	b := NewFromString("hello\nworld")
	b.Path = path
	b.Modified = true

	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if b.Modified {
		t.Error("expected Modified=false after save")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\nworld\n" {
		t.Errorf("file content: got %q", string(data))
	}
}

func TestClampOutOfBounds(t *testing.T) {
	b := NewFromString("hi")
	// Insert past end should clamp
	b.Insert(99, 99, "!")
	if b.LineText(0) != "hi!" {
		t.Errorf("got %q", b.LineText(0))
	}
}

// --- Origin Tests ---

func TestOriginDefaultsDeveloper(t *testing.T) {
	tests := []struct {
		name string
		buf  *Buffer
	}{
		{"New", New()},
		{"NewFromString", NewFromString("hello\nworld")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.buf.LineCount() {
				if got := tt.buf.LineOrigin(i); got != OriginDeveloper {
					t.Errorf("line %d: origin = %d, want OriginDeveloper", i, got)
				}
			}
		})
	}
}

func TestOriginOutOfBounds(t *testing.T) {
	b := NewFromString("hello")
	if got := b.LineOrigin(-1); got != OriginDeveloper {
		t.Errorf("negative index: got %d, want OriginDeveloper", got)
	}
	if got := b.LineOrigin(99); got != OriginDeveloper {
		t.Errorf("past end: got %d, want OriginDeveloper", got)
	}
}

func TestSetLineOrigin(t *testing.T) {
	b := NewFromString("hello\nworld")
	b.SetLineOrigin(1, OriginAgent)
	if got := b.LineOrigin(0); got != OriginDeveloper {
		t.Errorf("line 0: got %d, want OriginDeveloper", got)
	}
	if got := b.LineOrigin(1); got != OriginAgent {
		t.Errorf("line 1: got %d, want OriginAgent", got)
	}
}

func TestSetLineOrigins(t *testing.T) {
	b := NewFromString("a\nb\nc\nd")
	b.SetLineOrigins(1, 2, OriginAgent)
	expected := []Origin{OriginDeveloper, OriginAgent, OriginAgent, OriginDeveloper}
	for i, want := range expected {
		if got := b.LineOrigin(i); got != want {
			t.Errorf("line %d: got %d, want %d", i, got, want)
		}
	}
}

func TestInsertNewlineInheritsOrigin(t *testing.T) {
	b := NewFromString("hello world")
	b.SetLineOrigin(0, OriginAgent)
	// Split the agent line
	b.Insert(0, 5, "\n")
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Errorf("line 0: got %d, want OriginAgent", got)
	}
	if got := b.LineOrigin(1); got != OriginAgent {
		t.Errorf("line 1 (split): got %d, want OriginAgent", got)
	}
}

func TestInsertMultiLineInheritsOrigin(t *testing.T) {
	b := NewFromString("ab")
	b.SetLineOrigin(0, OriginAgent)
	b.Insert(0, 1, "X\nY\nZ")
	// All 3 lines should be OriginAgent (parent was agent)
	for i := range 3 {
		if got := b.LineOrigin(i); got != OriginAgent {
			t.Errorf("line %d: got %d, want OriginAgent", i, got)
		}
	}
}

func TestDeleteAcrossNewlinePreservesFirstOrigin(t *testing.T) {
	b := NewFromString("hello\nworld")
	b.SetLineOrigin(0, OriginAgent)
	b.SetLineOrigin(1, OriginDeveloper)
	// Join: delete newline at end of line 0
	b.Delete(0, 5, 1)
	if b.LineCount() != 1 {
		t.Fatalf("expected 1 line, got %d", b.LineCount())
	}
	// Merged line should retain first line's origin (Agent)
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Errorf("merged line: got %d, want OriginAgent", got)
	}
}

func TestInsertSingleLinePreservesOrigin(t *testing.T) {
	b := NewFromString("hello")
	b.SetLineOrigin(0, OriginAgent)
	b.Insert(0, 5, " world")
	// Single-line insert doesn't change origin
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Errorf("got %d, want OriginAgent", got)
	}
}

func TestUndoSetLineOrigin(t *testing.T) {
	b := NewFromString("hello")
	b.SetLineOrigin(0, OriginAgent)
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Fatalf("after set: got %d, want OriginAgent", got)
	}
	_, _, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if got := b.LineOrigin(0); got != OriginDeveloper {
		t.Errorf("after undo: got %d, want OriginDeveloper", got)
	}
}

func TestRedoSetLineOrigin(t *testing.T) {
	b := NewFromString("hello")
	b.SetLineOrigin(0, OriginAgent)
	b.Undo()
	_, _, ok := b.Redo()
	if !ok {
		t.Fatal("redo failed")
	}
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Errorf("after redo: got %d, want OriginAgent", got)
	}
}

func TestGroupedOriginUndoRedo(t *testing.T) {
	// Simulates an agent edit: grouped text change + origin change
	b := NewFromString("old text")
	b.BeginGroup()
	b.Delete(0, 0, 8) // delete "old text"
	b.Insert(0, 0, "new text")
	b.SetLineOrigin(0, OriginAgent)
	b.EndGroup()

	if b.LineText(0) != "new text" {
		t.Fatalf("after edit: got %q", b.LineText(0))
	}
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Fatalf("after edit: origin = %d, want OriginAgent", got)
	}

	// Undo the entire group — text AND origin should revert
	_, _, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if b.LineText(0) != "old text" {
		t.Errorf("after undo: got %q", b.LineText(0))
	}
	if got := b.LineOrigin(0); got != OriginDeveloper {
		t.Errorf("after undo: origin = %d, want OriginDeveloper", got)
	}

	// Redo — text AND origin should come back
	_, _, ok = b.Redo()
	if !ok {
		t.Fatal("redo failed")
	}
	if b.LineText(0) != "new text" {
		t.Errorf("after redo: got %q", b.LineText(0))
	}
	if got := b.LineOrigin(0); got != OriginAgent {
		t.Errorf("after redo: origin = %d, want OriginAgent", got)
	}
}

func TestGroupedMultiLineOriginUndo(t *testing.T) {
	// Agent inserts multiple lines, all marked as agent origin
	b := NewFromString("before\nafter")
	b.BeginGroup()
	b.Insert(0, 6, "\nline1\nline2")
	b.SetLineOrigins(1, 2, OriginAgent)
	b.EndGroup()

	if b.LineCount() != 4 {
		t.Fatalf("after insert: %d lines, want 4", b.LineCount())
	}
	if got := b.LineOrigin(1); got != OriginAgent {
		t.Fatalf("line 1: origin = %d, want OriginAgent", got)
	}
	if got := b.LineOrigin(2); got != OriginAgent {
		t.Fatalf("line 2: origin = %d, want OriginAgent", got)
	}

	// Undo — lines removed, origins gone
	_, _, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if b.LineCount() != 2 {
		t.Errorf("after undo: %d lines, want 2", b.LineCount())
	}
	if got := b.LineOrigin(0); got != OriginDeveloper {
		t.Errorf("line 0 after undo: origin = %d, want OriginDeveloper", got)
	}
	if got := b.LineOrigin(1); got != OriginDeveloper {
		t.Errorf("line 1 after undo: origin = %d, want OriginDeveloper", got)
	}
}

func TestSetLineOriginNoOpSkipsUndo(t *testing.T) {
	b := NewFromString("hello")
	// Setting to the same value should be a no-op
	b.SetLineOrigin(0, OriginDeveloper)
	_, _, ok := b.Undo()
	if ok {
		t.Error("expected no undo entry for no-op SetLineOrigin")
	}
}

func TestUndoDeleteRestoresPerLineOrigins(t *testing.T) {
	b := NewFromString("hello\nworld\nfoo")
	b.SetLineOrigin(0, OriginDeveloper)
	b.SetLineOrigin(1, OriginAgent)
	b.SetLineOrigin(2, OriginDeveloper)

	// Delete across newline: merges lines 0-2 into one
	b.Delete(0, 5, 5) // deletes "\nworl"
	if b.LineCount() != 2 {
		t.Fatalf("after delete: %d lines, want 2", b.LineCount())
	}

	// Undo should restore all three lines with original origins
	b.Undo()
	if b.LineCount() != 3 {
		t.Fatalf("after undo: %d lines, want 3", b.LineCount())
	}
	if got := b.LineOrigin(0); got != OriginDeveloper {
		t.Errorf("line 0: origin = %d, want OriginDeveloper", got)
	}
	if got := b.LineOrigin(1); got != OriginAgent {
		t.Errorf("line 1: origin = %d, want OriginAgent", got)
	}
	if got := b.LineOrigin(2); got != OriginDeveloper {
		t.Errorf("line 2: origin = %d, want OriginDeveloper", got)
	}
}

func TestGroupedRedoCursorNotCorruptedByOriginOps(t *testing.T) {
	b := NewFromString("old text")

	// Simulate an agent edit: grouped delete + insert + origin change
	b.BeginGroup()
	b.Delete(0, 0, 8)
	b.Insert(0, 0, "new text")
	b.SetLineOrigin(0, OriginAgent)
	b.EndGroup()

	// Undo the group
	b.Undo()

	// Redo — cursor should be at end of "new text" (0, 8), not (0, 0)
	l, c, ok := b.Redo()
	if !ok {
		t.Fatal("redo failed")
	}
	if l != 0 || c != 8 {
		t.Errorf("redo cursor: got (%d,%d), want (0,8)", l, c)
	}
}

func TestGroupedUndoCursorNotCorruptedByOriginOps(t *testing.T) {
	b := NewFromString("old text")

	b.BeginGroup()
	b.Delete(0, 0, 8)
	b.Insert(0, 0, "new text")
	b.SetLineOrigin(0, OriginAgent)
	b.EndGroup()

	// Undo — cursor should be at end of restored "old text" (0, 8), not (0, 0)
	l, c, ok := b.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	if l != 0 || c != 8 {
		t.Errorf("undo cursor: got (%d,%d), want (0,8)", l, c)
	}
}
