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
