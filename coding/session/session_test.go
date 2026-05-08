package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
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
func (stubWorkspace) CanonPath(p string) string         { return p }
func (stubWorkspace) InContext(_ string) bool           { return true }
func (stubWorkspace) AddContext(_ string)               {}
func (stubWorkspace) ProjectRoot() string               { return "" }

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
	e := openfile.New(buf)
	sess := New(e, projectRoot)
	events := make(chan event.Event, 64)
	ag := agent.New(stubProvider{}, stubWorkspace{}, events, nil)
	sess.SetAgent(ag, events)
	return sess
}

//nolint:gocognit,funlen // table-driven test — complexity comes from test cases, not logic
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
				s.pendingEdit = &event.PendingEdit{Search: "hello", Replace: "goodbye"}
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
				s.pendingEdit = &event.PendingEdit{Search: "missing", Replace: "found"}
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
				s.pendingEdit = &event.PendingEdit{Search: "hello", Replace: "goodbye"}
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
				s.pendingEdit = &event.PendingEdit{Search: "hello", Replace: "goodbye"}
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
				s.pendingEdit = &event.PendingEdit{Search: "world", Replace: "earth"}
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
				s.pendingEdit = &event.PendingEdit{Search: "line two", Replace: "LINE TWO"}
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
				if !strings.Contains(err.Error(), tt.wantErr) {
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
			if s.pendingEdit != nil {
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
	s.pendingEdit = &event.PendingEdit{Search: "hello", Replace: "goodbye"}
	s.editReviewed = true

	_, err := s.PrepareApproval("hello", "goodbye")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := s.activeOpenFile.Buf.Content()
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
	s.pendingEdit = &event.PendingEdit{Search: "missing", Replace: "found"}
	s.editReviewed = true

	_, err := s.PrepareApproval("missing", "found")
	if err == nil {
		t.Fatal("expected error for missing search text")
	}
	// Agent should have been signaled to reject (via agent.Reject)
	// and pending state should be cleared.
	if s.pendingEdit != nil {
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
	got := s.activeOpenFile.Buf.Content()
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
	eA := openfile.New(bufA)
	s := New(eA, dir)

	// Switch to B — opens from disk.
	if err := s.SwitchTo(pathB); err != nil {
		t.Fatal(err)
	}
	if s.activeOpenFile.Buf.Content() != "file B content" {
		t.Errorf("after switch to B: got %q", s.activeOpenFile.Buf.Content())
	}

	// Switch back to A — should reuse the existing buffer, not re-read disk.
	eA.Buf.Insert(0, 0, "!") // modify buffer A while viewing B
	if err := s.SwitchTo(pathA); err != nil {
		t.Fatal(err)
	}
	got := s.activeOpenFile.Buf.Content()
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
	s := New(openfile.New(bufA), dir)

	// Open second file.
	if err := s.SwitchTo(pathB); err != nil {
		t.Fatal(err)
	}

	// No modifications yet.
	if len(s.ModifiedFiles()) != 0 {
		t.Fatalf("expected no modified files, got %v", s.ModifiedFiles())
	}

	// Modify file B.
	s.activeOpenFile.Buf.Insert(0, 0, "X")
	modified := s.ModifiedFiles()
	if len(modified) != 1 {
		t.Fatalf("expected 1 modified file, got %v", modified)
	}
}

func TestOpenFileForPath(t *testing.T) {
	dir := t.TempDir()
	s := newTestSessionWithRoot("content", dir)

	path := dir + "/test.go"
	if err := writeTestFile(path, "package main"); err != nil {
		t.Fatal(err)
	}

	if err := s.SwitchTo(path); err != nil {
		t.Fatal(err)
	}

	of := s.OpenFileForPath(path)
	if of == nil {
		t.Fatal("OpenFileForPath returned nil for open file")
	}

	if s.OpenFileForPath("/nonexistent") != nil {
		t.Error("OpenFileForPath should return nil for unopened file")
	}
}

func TestSwitchToBlockedByPendingEdit(t *testing.T) {
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
	s := New(openfile.New(bufA), dir)
	events := make(chan event.Event, 64)
	ag := agent.New(stubProvider{}, stubWorkspace{}, events, nil)
	s.SetAgent(ag, events)

	// With a pending edit, SwitchTo should return ErrEditPending.
	s.pendingEdit = &event.PendingEdit{Search: "file A", Replace: "changed"}
	err = s.SwitchTo(pathB)
	if !errors.Is(err, ErrEditPending) {
		t.Fatalf("SwitchTo with PendingEdit: got %v, want ErrEditPending", err)
	}

	// Clear PendingEdit, simulate PrepareApproval state (stagedEditFile set).
	s.pendingEdit = nil
	s.editReviewed = false
	s.stagedEditFile = pathA
	err = s.SwitchTo(pathB)
	if !errors.Is(err, ErrEditPending) {
		t.Fatalf("SwitchTo with stagedEditFile: got %v, want ErrEditPending", err)
	}

	// Clear both — SwitchTo should succeed.
	s.stagedEditFile = ""
	err = s.SwitchTo(pathB)
	if err != nil {
		t.Fatalf("SwitchTo after clearing: unexpected error: %v", err)
	}
	if s.ActiveFile() != pathB {
		t.Errorf("ActiveFile = %q, want %q", s.ActiveFile(), pathB)
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
			_, err := s.resolvePath(tt.path)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for path %q, got nil", tt.path)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for path %q: %v", tt.path, err)
			}
		})
	}
}

func TestContextSetAddRemove(t *testing.T) {
	dir := t.TempDir()
	s := newTestSessionWithRoot("", dir)

	s.AddContext("src/main.go")
	if !s.InContext("src/main.go") {
		t.Error("expected src/main.go in context after AddContext")
	}

	files := s.ContextFiles()
	if len(files) != 1 || files[0] != "src/main.go" {
		t.Errorf("ContextFiles = %v, want [src/main.go]", files)
	}

	s.RemoveContext("src/main.go")
	if s.InContext("src/main.go") {
		t.Error("expected src/main.go removed from context")
	}
	if len(s.ContextFiles()) != 0 {
		t.Errorf("ContextFiles = %v, want empty", s.ContextFiles())
	}
}

func TestContextSetAutoAddOnNew(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.go"
	if err := writeTestFile(path, "package main"); err != nil {
		t.Fatal(err)
	}

	buf, err := buffer.NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(openfile.New(buf), dir)
	if !s.InContext(path) {
		t.Error("initial file should be auto-added to context")
	}
}

func TestContextSetAutoAddOnSwitchTo(t *testing.T) {
	dir := t.TempDir()
	pathA := dir + "/a.go"
	pathB := dir + "/b.go"
	if err := writeTestFile(pathA, "package a"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(pathB, "package b"); err != nil {
		t.Fatal(err)
	}

	bufA, err := buffer.NewFromFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	s := New(openfile.New(bufA), dir)

	if err := s.SwitchTo(pathB); err != nil {
		t.Fatal(err)
	}
	if !s.InContext(pathB) {
		t.Error("SwitchTo should auto-add file to context")
	}

	files := s.ContextFiles()
	if len(files) != 2 {
		t.Errorf("ContextFiles count = %d, want 2", len(files))
	}
}

func TestContextSetPersistence(t *testing.T) {
	dir := t.TempDir()

	// Create session, add context, verify file written
	s1 := newTestSessionWithRoot("", dir)
	s1.AddContext("src/main.go")
	s1.AddContext("src/util.go")

	contextFile := dir + "/.project/context.md"
	data, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("context.md not created: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "- src/main.go") {
		t.Errorf("context.md missing src/main.go: %s", content)
	}
	if !strings.Contains(content, "- src/util.go") {
		t.Errorf("context.md missing src/util.go: %s", content)
	}

	// Create new session from same root — should load persisted context
	s2 := newTestSessionWithRoot("", dir)
	if !s2.InContext("src/main.go") {
		t.Error("src/main.go not loaded from persisted context")
	}
	if !s2.InContext("src/util.go") {
		t.Error("src/util.go not loaded from persisted context")
	}
}

func TestContextSetAutoAddOnWriteFile(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	newPath := dir + "/created.go"
	if err := s.WriteFile("created.go", "package created"); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	if !s.InContext(newPath) {
		t.Error("WriteFile should auto-add created file to context")
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// --- Provenance Tests ---

func TestApproveEditMarksAgentOrigin(t *testing.T) {
	s := newTestSession("old text")

	// Simulate agent proposing an edit
	s.pendingEdit = &event.PendingEdit{Search: "old text", Replace: "new text"}
	s.ReviewEdit()

	// Drain the approve signal in a goroutine (agent.Approve sends to channel)
	ok, reason := s.ApproveEdit("old text", "new text")
	if !ok {
		t.Fatalf("ApproveEdit failed: %s", reason)
	}

	// Line should be marked as agent-written
	if got := s.activeOpenFile.Buf.LineOrigin(0); got != buffer.OriginAgent {
		t.Errorf("line 0 origin = %d, want OriginAgent", got)
	}
}

func TestApproveEditTracksModifiedFile(t *testing.T) {
	root := t.TempDir()
	filePath := root + "/test.go"
	if err := writeTestFile(filePath, "old\n"); err != nil {
		t.Fatal(err)
	}

	buf, err := buffer.NewFromFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	e := openfile.New(buf)
	s := New(e, root)
	events := make(chan event.Event, 64)
	ag := agent.New(stubProvider{}, stubWorkspace{}, events, nil)
	s.SetAgent(ag, events)

	s.pendingEdit = &event.PendingEdit{Search: "old", Replace: "new"}
	s.ReviewEdit()
	ok, _ := s.ApproveEdit("old", "new")
	if !ok {
		t.Fatal("ApproveEdit failed")
	}

	// File should be in agent-modified list
	modified := s.AgentModifiedFiles()
	if len(modified) != 1 {
		t.Fatalf("AgentModifiedFiles: got %d files, want 1", len(modified))
	}
	if !strings.HasSuffix(modified[0], "test.go") {
		t.Errorf("AgentModifiedFiles[0] = %q, want test.go", modified[0])
	}
}

func TestFileStatus(t *testing.T) {
	root := t.TempDir()
	filePath := root + "/main.go"
	if err := writeTestFile(filePath, "code\n"); err != nil {
		t.Fatal(err)
	}

	buf, err := buffer.NewFromFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	e := openfile.New(buf)
	s := New(e, root)
	events := make(chan event.Event, 64)
	ag := agent.New(stubProvider{}, stubWorkspace{}, events, nil)
	s.SetAgent(ag, events)

	// Initially: in context (auto-added), not modified
	inCtx, agentMod := s.FileStatus(filePath)
	if !inCtx {
		t.Error("expected inContext=true for initial file")
	}
	if agentMod {
		t.Error("expected agentModified=false before any agent edit")
	}

	// After agent edit: should be modified
	s.pendingEdit = &event.PendingEdit{Search: "code", Replace: "new code"}
	s.ReviewEdit()
	s.ApproveEdit("code", "new code")

	inCtx, agentMod = s.FileStatus(filePath)
	if !inCtx {
		t.Error("expected inContext=true after edit")
	}
	if !agentMod {
		t.Error("expected agentModified=true after agent edit")
	}
}

func TestDeveloperEditResetsAgentOrigin(t *testing.T) {
	s := newTestSession("agent line")

	// Simulate agent writing a line
	s.activeOpenFile.Buf.SetLineOrigin(0, buffer.OriginAgent)
	if got := s.activeOpenFile.Buf.LineOrigin(0); got != buffer.OriginAgent {
		t.Fatalf("setup: origin = %d, want OriginAgent", got)
	}

	// Developer types on the agent line. Mirrors editor.InsertChar:
	// reset the affected line's origin to Developer, then insert at
	// the cursor position. Cursor lives on the TUI's editor controller
	// post-Phase C, so we operate on the buffer directly.
	s.activeOpenFile.Buf.ResetOriginToDeveloper(0)
	s.activeOpenFile.Buf.Insert(0, 5, "X")

	// Origin should flip to Developer
	if got := s.activeOpenFile.Buf.LineOrigin(0); got != buffer.OriginDeveloper {
		t.Errorf("after developer edit: origin = %d, want OriginDeveloper", got)
	}
}

func TestAgentOriginSurvivesUndoRedo(t *testing.T) {
	s := newTestSession("original")

	// Simulate a grouped agent edit with origin
	s.activeOpenFile.Buf.BeginGroup()
	s.activeOpenFile.Buf.Delete(0, 0, 8)
	s.activeOpenFile.Buf.InsertWithOrigin(0, 0, "replaced", buffer.OriginAgent)
	s.activeOpenFile.Buf.EndGroup()

	if got := s.activeOpenFile.Buf.LineOrigin(0); got != buffer.OriginAgent {
		t.Fatalf("after edit: origin = %d, want OriginAgent", got)
	}

	// Undo should restore Developer origin
	s.activeOpenFile.Buf.Undo()
	if s.activeOpenFile.Buf.LineText(0) != "original" {
		t.Fatalf("after undo: text = %q", s.activeOpenFile.Buf.LineText(0))
	}
	if got := s.activeOpenFile.Buf.LineOrigin(0); got != buffer.OriginDeveloper {
		t.Errorf("after undo: origin = %d, want OriginDeveloper", got)
	}

	// Redo should restore Agent origin
	s.activeOpenFile.Buf.Redo()
	if got := s.activeOpenFile.Buf.LineOrigin(0); got != buffer.OriginAgent {
		t.Errorf("after redo: origin = %d, want OriginAgent", got)
	}
}

func TestPrepareApprovalDevModifiedReplace(t *testing.T) {
	s := newTestSession("hello world")

	// Agent proposes replacing "world" with "earth"
	s.pendingEdit = &event.PendingEdit{
		Search:  "world",
		Replace: "earth",
	}
	s.ReviewEdit()

	// Developer modifies the replacement in the overlay to "mars"
	plan, err := s.PrepareApproval("world", "mars")
	if err != nil {
		t.Fatalf("PrepareApproval: %v", err)
	}

	// Single-line replacement: "world" != "mars" → Developer
	if len(plan.LineOrigins) != 1 {
		t.Fatalf("LineOrigins len = %d, want 1", len(plan.LineOrigins))
	}
	if plan.LineOrigins[0] == nil || *plan.LineOrigins[0] != buffer.OriginDeveloper {
		t.Errorf("LineOrigins[0] = %v, want OriginDeveloper", plan.LineOrigins[0])
	}
}

func TestPrepareApprovalUnmodifiedIsAgent(t *testing.T) {
	s := newTestSession("hello world")

	// Agent proposes replacing "world" with "earth"
	s.pendingEdit = &event.PendingEdit{
		Search:  "world",
		Replace: "earth",
	}
	s.ReviewEdit()

	// Developer approves without modification
	plan, err := s.PrepareApproval("world", "earth")
	if err != nil {
		t.Fatalf("PrepareApproval: %v", err)
	}

	// "world" != "earth" → Agent (line changed)
	if len(plan.LineOrigins) != 1 {
		t.Fatalf("LineOrigins len = %d, want 1", len(plan.LineOrigins))
	}
	if plan.LineOrigins[0] == nil || *plan.LineOrigins[0] != buffer.OriginAgent {
		t.Errorf("LineOrigins[0] = %v, want OriginAgent", plan.LineOrigins[0])
	}
}

func TestPrepareApprovalPerLineOrigins(t *testing.T) {
	s := newTestSession("old\ncode\nhere")

	// Agent proposes 3-line replacement, only changes line 0
	s.pendingEdit = &event.PendingEdit{
		Search:  "old\ncode\nhere",
		Replace: "new\ncode\nhere",
	}
	s.ReviewEdit()

	// Developer also modifies line 1
	plan, err := s.PrepareApproval("old\ncode\nhere", "new\nmodified\nhere")
	if err != nil {
		t.Fatalf("PrepareApproval: %v", err)
	}

	if len(plan.LineOrigins) != 3 {
		t.Fatalf("LineOrigins len = %d, want 3", len(plan.LineOrigins))
	}

	// Line 0: search="old", final="new" → changed → agent original was "new", final is "new" → Agent
	if plan.LineOrigins[0] == nil || *plan.LineOrigins[0] != buffer.OriginAgent {
		t.Errorf("LineOrigins[0] = %v, want OriginAgent", plan.LineOrigins[0])
	}
	// Line 1: search="code", final="modified" → changed → agent original was "code", final is "modified" → Developer
	if plan.LineOrigins[1] == nil || *plan.LineOrigins[1] != buffer.OriginDeveloper {
		t.Errorf("LineOrigins[1] = %v, want OriginDeveloper", plan.LineOrigins[1])
	}
	// Line 2: search="here", final="here" → unchanged → nil (skip)
	if plan.LineOrigins[2] != nil {
		t.Errorf("LineOrigins[2] = %v, want nil (unchanged line)", plan.LineOrigins[2])
	}
}

func TestDeleteFile(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	// Create file first
	if err := s.WriteFile("victim.go", "package victim"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	absPath := dir + "/victim.go"
	canon := s.CanonPath(absPath)

	// Verify it exists and is in context
	if _, err := os.Stat(absPath); err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if !s.InContext(canon) {
		t.Fatal("file should be in context after WriteFile")
	}

	// Delete it
	if err := s.DeleteFile(absPath); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// File should be gone from disk
	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Errorf("file should not exist after DeleteFile, got err=%v", err)
	}

	// Should be removed from context
	if s.InContext(canon) {
		t.Error("file should not be in context after DeleteFile")
	}

	// Should be removed from editors
	s.mu.RLock()
	_, hasEditor := s.openFiles[canon]
	s.mu.RUnlock()
	if hasEditor {
		t.Error("editor should be removed after DeleteFile")
	}
}

func TestDeleteFile_ActiveFile(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	// Create and switch to a file
	if err := s.WriteFile("active.go", "package active"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	absPath := dir + "/active.go"
	if err := s.SwitchTo(absPath); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}

	// Delete the active file
	if err := s.DeleteFile(absPath); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// activeFile should be cleared (no other named editors remain)
	if s.ActiveFile() != "" {
		t.Errorf("ActiveFile should be empty after deleting active file, got %q", s.ActiveFile())
	}
	// Active open-file must never be nil — a fresh empty handle is
	// installed as fallback so callers (intent.go reads
	// activeOpenFile.Content() unconditionally) cannot panic.
	if s.ActiveOpenFile() == nil {
		t.Fatal("ActiveOpenFile should not be nil after deleting active file")
	}
}

func TestDeleteFile_Directory(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	// Create files inside a subdirectory
	if err := s.WriteFile("pkg/a.go", "package pkg"); err != nil {
		t.Fatalf("WriteFile a.go: %v", err)
	}
	if err := s.WriteFile("pkg/b.go", "package pkg"); err != nil {
		t.Fatalf("WriteFile b.go: %v", err)
	}

	// Both should be in context
	canonA := s.CanonPath(filepath.Join(dir, "pkg/a.go"))
	canonB := s.CanonPath(filepath.Join(dir, "pkg/b.go"))
	if !s.InContext(canonA) || !s.InContext(canonB) {
		t.Fatal("files should be in context")
	}

	// Delete the directory
	if err := s.DeleteFile(filepath.Join(dir, "pkg")); err != nil {
		t.Fatalf("DeleteFile dir: %v", err)
	}

	// Directory should be gone
	if _, err := os.Stat(filepath.Join(dir, "pkg")); !os.IsNotExist(err) {
		t.Errorf("directory should not exist after delete, got err=%v", err)
	}

	// Both files should be removed from context and editors
	if s.InContext(canonA) {
		t.Error("a.go should not be in context after dir delete")
	}
	if s.InContext(canonB) {
		t.Error("b.go should not be in context after dir delete")
	}
	s.mu.RLock()
	_, hasA := s.openFiles[canonA]
	_, hasB := s.openFiles[canonB]
	s.mu.RUnlock()
	if hasA || hasB {
		t.Error("editors should be removed after dir delete")
	}
}

func TestDeleteFile_NonExistent(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	err := s.DeleteFile(dir + "/nope.go")
	if err == nil {
		t.Error("DeleteFile on non-existent file should return error")
	}
}

func TestDeleteFile_ProjectRoot(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	err := s.DeleteFile(".")
	if err == nil {
		t.Fatal("DeleteFile(\".\") should return error")
	}
	if !strings.Contains(err.Error(), "project root") {
		t.Errorf("error should mention project root, got: %v", err)
	}

	err = s.DeleteFile(dir)
	if err == nil {
		t.Fatal("DeleteFile(projectRoot) should return error")
	}
}

func TestDeleteFile_EscapesRoot(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	err := s.DeleteFile("../../etc/passwd")
	if err == nil {
		t.Error("DeleteFile with path escape should return error")
	}
}

func TestDeleteFile_ProjectMeta(t *testing.T) {
	dir := t.TempDir()
	s := New(openfile.New(buffer.New()), dir)

	// Create .project/context.md
	metaDir := filepath.Join(dir, ".project")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metaFile := filepath.Join(metaDir, "context.md")
	if err := os.WriteFile(metaFile, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := s.DeleteFile(metaFile)
	if err == nil {
		t.Error("DeleteFile should reject .project/ files")
	}
	if !strings.Contains(err.Error(), "project metadata") {
		t.Errorf("error should mention project metadata, got: %v", err)
	}

	// File should still exist
	if _, err := os.Stat(metaFile); err != nil {
		t.Errorf("file should still exist after rejected delete: %v", err)
	}

	// Deleting .project directory itself should also be rejected
	err = s.DeleteFile(metaDir)
	if err == nil {
		t.Error("DeleteFile should reject .project directory itself")
	}
	if _, err := os.Stat(metaDir); err != nil {
		t.Errorf(".project dir should still exist after rejected delete: %v", err)
	}
}
