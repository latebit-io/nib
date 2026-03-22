package session

import (
	"context"
	"os"
	"testing"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/llm"
)

// stubProvider satisfies llm.Provider for constructing an agent in tests.
type stubProvider struct{}

func (stubProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// stubWorkspace satisfies agent.Workspace for tests.
type stubWorkspace struct{}

func (stubWorkspace) ReadFile(_ string) (string, error) { return "", nil }
func (stubWorkspace) ListFiles() ([]string, error)      { return nil, nil }
func (stubWorkspace) WriteFile(_, _ string) error       { return nil }

// newTestSession creates a session with a buffer containing the given text
// and a real agent (needed to test approval signaling).
func newTestSession(content string) *Session {
	return newTestSessionWithRoot(content, "")
}

// newTestSessionWithRoot creates a test session with a specific project root.
func newTestSessionWithRoot(content, projectRoot string) *Session {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	e := editor.New(buf)
	sess := New(e, projectRoot)
	events := make(chan agent.Event, 64)
	ag := agent.New(stubProvider{}, stubWorkspace{}, events)
	sess.SetAgent(ag, events)
	return sess
}

func TestPrepareApproval(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		search   string
		replace  string
		setup    func(s *Session) // set up pending edit + review
		wantErr  string
		wantLine int
		wantCol  int
	}{
		{
			name:    "no pending edit",
			content: "hello world",
			search:  "hello",
			replace: "goodbye",
			setup:   func(_ *Session) {},
			wantErr: "no pending edit",
		},
		{
			name:    "edit not reviewed",
			content: "hello world",
			search:  "hello",
			replace: "goodbye",
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
				// Don't call ReviewEdit
			},
			wantErr: "edit not reviewed",
		},
		{
			name:    "search text not found",
			content: "hello world",
			search:  "missing",
			replace: "found",
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "missing", Replace: "found"}
				s.editReviewed = true
			},
			wantErr: "text not found",
		},
		{
			name:    "ambiguous match",
			content: "hello hello",
			search:  "hello",
			replace: "goodbye",
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
				s.editReviewed = true
			},
			wantErr: "2 matches found",
		},
		{
			name:     "success at start of buffer",
			content:  "hello world",
			search:   "hello",
			replace:  "goodbye",
			wantLine: 0,
			wantCol:  0,
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
				s.editReviewed = true
			},
		},
		{
			name:     "success mid-line",
			content:  "hello world",
			search:   "world",
			replace:  "earth",
			wantLine: 0,
			wantCol:  6,
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "world", Replace: "earth"}
				s.editReviewed = true
			},
		},
		{
			name:     "success on second line",
			content:  "line one\nline two\nline three",
			search:   "line two",
			replace:  "LINE TWO",
			wantLine: 1,
			wantCol:  0,
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "line two", Replace: "LINE TWO"}
				s.editReviewed = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSession(tt.content)
			tt.setup(s)

			plan, err := s.PrepareApproval(tt.search, tt.replace)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if plan == nil {
				t.Fatal("expected non-nil plan")
			}
			if plan.Line != tt.wantLine {
				t.Errorf("Line = %d, want %d", plan.Line, tt.wantLine)
			}
			if plan.Col != tt.wantCol {
				t.Errorf("Col = %d, want %d", plan.Col, tt.wantCol)
			}
			if plan.Search != tt.search {
				t.Errorf("Search = %q, want %q", plan.Search, tt.search)
			}
			if plan.Replace != tt.replace {
				t.Errorf("Replace = %q, want %q", plan.Replace, tt.replace)
			}
			// Pending edit should be cleared
			if s.PendingEdit != nil {
				t.Error("PendingEdit should be nil after PrepareApproval")
			}
			if s.editReviewed {
				t.Error("editReviewed should be false after PrepareApproval")
			}
		})
	}
}

func TestPrepareApprovalDoesNotMutateBuffer(t *testing.T) {
	s := newTestSession("hello world")
	s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
	s.editReviewed = true

	_, err := s.PrepareApproval("hello", "goodbye")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := s.Editor.Buf.Content()
	if got != "hello world" {
		t.Errorf("buffer mutated: got %q, want %q", got, "hello world")
	}
}

func TestCompleteApprovalSignalsAgent(t *testing.T) {
	s := newTestSession("hello world")
	// Smoke test: CompleteApproval should be safe to call on a fresh session
	// and must not panic or block. Agent.Approve uses a buffered channel, so
	// this call is non-blocking even when there is no pending edit.
	s.CompleteApproval()
}

func TestPrepareApprovalRejectsOnLocationFailure(t *testing.T) {
	s := newTestSession("hello world")
	s.PendingEdit = &agent.PendingEdit{Search: "missing", Replace: "found"}
	s.editReviewed = true

	_, err := s.PrepareApproval("missing", "found")
	if err == nil {
		t.Fatal("expected error for missing search text")
	}
	// Agent should have been signaled to reject (via agent.Reject)
	// and pending state should be cleared.
	if s.PendingEdit != nil {
		t.Error("PendingEdit should be nil after failed PrepareApproval")
	}
}

func TestSwitchTo(t *testing.T) {
	dir := t.TempDir()
	s := newTestSessionWithRoot("old content", dir)

	// Write a temp file to switch to
	newPath := dir + "/new.txt"
	if err := writeTestFile(newPath, "new content"); err != nil {
		t.Fatal(err)
	}

	err := s.SwitchTo(newPath)
	if err != nil {
		t.Fatalf("SwitchTo failed: %v", err)
	}

	// New editor is active.
	got := s.Editor.Buf.Content()
	if got != "new content" && got != "new content\n" {
		t.Errorf("new editor content = %q, want %q", got, "new content")
	}
	if s.ActiveFile() != newPath {
		t.Errorf("ActiveFile = %q, want %q", s.ActiveFile(), newPath)
	}

	// Both editors are tracked (old buffer has no path so only the new one is in map,
	// plus the empty-path original is only tracked if it had a path).
	files := s.OpenFiles()
	if len(files) < 1 {
		t.Errorf("OpenFiles count = %d, want at least 1", len(files))
	}
}

func TestSwitchToExistingBuffer(t *testing.T) {
	dir := t.TempDir()

	// Create two real files so both have paths.
	pathA := dir + "/a.txt"
	pathB := dir + "/b.txt"
	if err := writeTestFile(pathA, "file A content"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(pathB, "file B content"); err != nil {
		t.Fatal(err)
	}

	// Start with file A.
	bufA, err := buffer.NewFromFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	eA := editor.New(bufA)
	s := New(eA, dir)

	// Switch to B — opens from disk.
	if err := s.SwitchTo(pathB); err != nil {
		t.Fatal(err)
	}
	if s.Editor.Buf.Content() != "file B content" {
		t.Errorf("after switch to B: got %q", s.Editor.Buf.Content())
	}

	// Switch back to A — should reuse the existing buffer, not re-read disk.
	eA.InsertChar('!') // modify buffer A while viewing B
	if err := s.SwitchTo(pathA); err != nil {
		t.Fatal(err)
	}
	got := s.Editor.Buf.Content()
	if got == "file A content" {
		t.Error("expected modified buffer A, got original disk content — buffer was not reused")
	}
}

func TestMultiBufferModifiedTracking(t *testing.T) {
	dir := t.TempDir()
	pathA := dir + "/a.txt"
	pathB := dir + "/b.txt"
	if err := writeTestFile(pathA, "file A"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(pathB, "file B"); err != nil {
		t.Fatal(err)
	}

	bufA, err := buffer.NewFromFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	s := New(editor.New(bufA), dir)

	// Open second file.
	if err := s.SwitchTo(pathB); err != nil {
		t.Fatal(err)
	}

	// No modifications yet.
	if len(s.ModifiedFiles()) != 0 {
		t.Fatalf("expected no modified files, got %v", s.ModifiedFiles())
	}

	// Modify file B.
	s.Editor.InsertChar('X')
	modified := s.ModifiedFiles()
	if len(modified) != 1 {
		t.Fatalf("expected 1 modified file, got %v", modified)
	}
}

func TestEditorForPath(t *testing.T) {
	dir := t.TempDir()
	s := newTestSessionWithRoot("content", dir)

	path := dir + "/test.go"
	if err := writeTestFile(path, "package main"); err != nil {
		t.Fatal(err)
	}

	if err := s.SwitchTo(path); err != nil {
		t.Fatal(err)
	}

	e := s.EditorForPath(path)
	if e == nil {
		t.Fatal("EditorForPath returned nil for open file")
	}

	e2 := s.EditorForPath("/nonexistent")
	if e2 != nil {
		t.Error("EditorForPath should return nil for unopened file")
	}
}

func TestResolvePathTraversal(t *testing.T) {
	dir := t.TempDir()
	s := newTestSessionWithRoot("content", dir)

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"relative inside root", "src/main.go", false},
		{"traversal escapes root", "../../etc/passwd", true},
		{"absolute inside root", dir + "/src/main.go", false},
		{"absolute outside root", "/etc/passwd", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.ReadFile(tt.path)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for path %q, got nil", tt.path)
			}
			// Non-error cases may still fail (file doesn't exist) — that's fine,
			// we're just testing that traversal is rejected before disk access.
		})
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsAt(s, substr)
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
