package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/editor"
	"github.com/latebit-io/nib/engine/lang"
)

// mockDefinitionProvider implements lang.DocumentSyncer + lang.DefinitionProvider.
type mockDefinitionProvider struct {
	loc lang.Location
	err error
}

func (m *mockDefinitionProvider) DidOpen(string, string, string)      {}
func (m *mockDefinitionProvider) DidChange(string, []lang.TextChange) {}
func (m *mockDefinitionProvider) DidSave(string)                      {}
func (m *mockDefinitionProvider) DidClose(string)                     {}
func (m *mockDefinitionProvider) Close() error                        { return nil }
func (m *mockDefinitionProvider) Definition(_ context.Context, _ string, _, _ int) (lang.Location, error) {
	return m.loc, m.err
}

// mockHoverProvider implements lang.DocumentSyncer + lang.HoverProvider.
type mockHoverProvider struct {
	text string
	err  error
}

func (m *mockHoverProvider) DidOpen(string, string, string)      {}
func (m *mockHoverProvider) DidChange(string, []lang.TextChange) {}
func (m *mockHoverProvider) DidSave(string)                      {}
func (m *mockHoverProvider) DidClose(string)                     {}
func (m *mockHoverProvider) Close() error                        { return nil }
func (m *mockHoverProvider) Hover(_ context.Context, _ string, _, _ int) (string, error) {
	return m.text, m.err
}

// newNavTestSession creates a session with a temp file so ActiveFile() works.
func newNavTestSession(t *testing.T, content string) *Session {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	buf, err := buffer.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e := editor.New(buf)
	return New(e, dir)
}

// lookupAndNavigate mirrors the live TUI path: LookupDefinition + PushNav + MoveCursorTo.
func lookupAndNavigate(t *testing.T, sess *Session, line, col int) *lang.Location {
	t.Helper()
	originPath := sess.ActiveFile()
	originLine, originCol := sess.activeEditor.CursorLine, sess.activeEditor.CursorCol

	loc, err := sess.LookupDefinition(line, col)
	if err != nil {
		t.Fatalf("LookupDefinition failed: %v", err)
	}

	sess.PushNav(originPath, originLine, originCol)

	if loc.Path != sess.ActiveFile() {
		if err := sess.SwitchTo(loc.Path); err != nil {
			sess.PopNav()
			t.Fatalf("SwitchTo failed: %v", err)
		}
	}
	sess.activeEditor.MoveCursorTo(loc.Line, loc.Col)
	return loc
}

func TestLookupDefinition(t *testing.T) {
	t.Run("returns location", func(t *testing.T) {
		sess := newNavTestSession(t, "line0\nline1\nline2\n")
		mock := &mockDefinitionProvider{
			loc: lang.Location{Path: sess.ActiveFile(), Line: 2, Col: 3},
		}
		sess.SetLanguageService(mock)

		loc, err := sess.LookupDefinition(0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if loc.Line != 2 || loc.Col != 3 {
			t.Errorf("got %d:%d, want 2:3", loc.Line, loc.Col)
		}
	})

	t.Run("no language service", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		_, err := sess.LookupDefinition(0, 0)
		if err == nil {
			t.Fatal("expected error when no language service")
		}
	})

	t.Run("error propagates", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		mock := &mockDefinitionProvider{err: errors.New("no definition")}
		sess.SetLanguageService(mock)

		_, err := sess.LookupDefinition(0, 0)
		if err == nil || err.Error() != "no definition" {
			t.Fatalf("expected 'no definition' error, got: %v", err)
		}
	})
}

func TestNavigateAndGoBack(t *testing.T) {
	t.Run("same file jump and go-back", func(t *testing.T) {
		sess := newNavTestSession(t, "line0\nline1\nline2\n")
		sess.activeEditor.MoveCursorTo(1, 2)
		mock := &mockDefinitionProvider{
			loc: lang.Location{Path: sess.ActiveFile(), Line: 2, Col: 0},
		}
		sess.SetLanguageService(mock)

		lookupAndNavigate(t, sess, 1, 2)
		if sess.activeEditor.CursorLine != 2 {
			t.Fatalf("cursor should be at line 2, got %d", sess.activeEditor.CursorLine)
		}

		loc := sess.GoBack()
		if loc == nil {
			t.Fatal("GoBack returned nil")
		}
		if loc.Line != 1 || loc.Col != 2 {
			t.Errorf("GoBack returned %d:%d, want 1:2", loc.Line, loc.Col)
		}
		if sess.activeEditor.CursorLine != 1 || sess.activeEditor.CursorCol != 2 {
			t.Errorf("cursor at %d:%d, want 1:2", sess.activeEditor.CursorLine, sess.activeEditor.CursorCol)
		}
	})

	t.Run("empty stack returns nil", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		if sess.GoBack() != nil {
			t.Error("expected nil from empty stack")
		}
	})

	t.Run("LIFO order", func(t *testing.T) {
		sess := newNavTestSession(t, "line0\nline1\nline2\nline3\n")
		mock := &mockDefinitionProvider{}
		sess.SetLanguageService(mock)

		// Jump 0:0 → 1:0
		sess.activeEditor.MoveCursorTo(0, 0)
		mock.loc = lang.Location{Path: sess.ActiveFile(), Line: 1, Col: 0}
		lookupAndNavigate(t, sess, 0, 0)

		// Jump 1:0 → 3:0
		mock.loc = lang.Location{Path: sess.ActiveFile(), Line: 3, Col: 0}
		lookupAndNavigate(t, sess, 1, 0)

		// First go-back → 1:0
		loc := sess.GoBack()
		if loc == nil || loc.Line != 1 {
			t.Errorf("first go-back: want line 1, got %+v", loc)
		}

		// Second go-back → 0:0
		loc = sess.GoBack()
		if loc == nil || loc.Line != 0 {
			t.Errorf("second go-back: want line 0, got %+v", loc)
		}

		// Stack empty
		if sess.GoBack() != nil {
			t.Error("stack should be empty")
		}
	})

	t.Run("PopNav undoes PushNav", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		sess.PushNav(sess.ActiveFile(), 5, 10)
		sess.PopNav()
		if sess.GoBack() != nil {
			t.Error("stack should be empty after PopNav")
		}
	})
}

func TestHoverInfo(t *testing.T) {
	t.Run("returns hover text", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		mock := &mockHoverProvider{text: "func main()"}
		sess.SetLanguageService(mock)

		text, err := sess.HoverInfo(0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "func main()" {
			t.Errorf("got %q, want %q", text, "func main()")
		}
	})

	t.Run("no language service", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		_, err := sess.HoverInfo(0, 0)
		if err == nil {
			t.Fatal("expected error when no language service")
		}
	})

	t.Run("empty hover is not an error", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		mock := &mockHoverProvider{text: ""}
		sess.SetLanguageService(mock)

		text, err := sess.HoverInfo(0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "" {
			t.Errorf("expected empty string, got %q", text)
		}
	})
}
