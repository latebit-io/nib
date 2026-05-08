package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/engine/openfile"
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
	return New(openfile.New(buf), dir)
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

// TestNavigateAndGoBack exercises Session's nav-stack contract: GoBack
// returns the popped location; the frontend is responsible for cursor
// placement on its own editor instance after the file switch lands.
func TestNavigateAndGoBack(t *testing.T) {
	t.Run("same-file go-back returns pushed location", func(t *testing.T) {
		sess := newNavTestSession(t, "line0\nline1\nline2\n")
		sess.PushNav(sess.ActiveFile(), 1, 2)

		loc := sess.GoBack()
		if loc == nil {
			t.Fatal("GoBack returned nil")
		}
		if loc.Line != 1 || loc.Col != 2 {
			t.Errorf("GoBack returned %d:%d, want 1:2", loc.Line, loc.Col)
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
		path := sess.ActiveFile()
		sess.PushNav(path, 0, 0)
		sess.PushNav(path, 1, 0)

		loc := sess.GoBack()
		if loc == nil || loc.Line != 1 {
			t.Errorf("first go-back: want line 1, got %+v", loc)
		}

		loc = sess.GoBack()
		if loc == nil || loc.Line != 0 {
			t.Errorf("second go-back: want line 0, got %+v", loc)
		}

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

// TestNavigateAgent verifies Session.NavigateAgent switches to the target
// file when needed and is a no-op when the target is already active.
// Cursor placement is the frontend's responsibility — verified in TUI tests.
func TestNavigateAgent(t *testing.T) {
	t.Run("no-op when path matches active file", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		active := sess.ActiveFile()
		if err := sess.NavigateAgent(active); err != nil {
			t.Fatalf("NavigateAgent: %v", err)
		}
		if sess.ActiveFile() != active {
			t.Errorf("active file changed unexpectedly")
		}
	})

	t.Run("switches when path differs", func(t *testing.T) {
		sess := newNavTestSession(t, "hello\n")
		dir := filepath.Dir(sess.ActiveFile())
		other := filepath.Join(dir, "other.go")
		if err := os.WriteFile(other, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := sess.NavigateAgent(other); err != nil {
			t.Fatalf("NavigateAgent: %v", err)
		}
		if sess.ActiveFile() != sess.CanonPath(other) {
			t.Errorf("active file = %q, want %q", sess.ActiveFile(), sess.CanonPath(other))
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
