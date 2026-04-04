package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/event"
)

// mockAgent simulates an agent for testing the runner's event handling.
// It sends pre-configured events on the shared channel when Run is called.
type mockAgent struct {
	events      chan<- event.Event
	runFunc     func() // custom behavior on Run — sends events to channel
	approved    bool
	continued   bool
	lastPath    string
	lastContent string
	cancelled   bool
	replied     bool

	// continueDone is signaled when Continue() is called. Tests that send
	// AgentEditProposed should wait on this before sending AgentDone to
	// avoid a race between applyEdit and the done event.
	continueDone chan struct{}

	// Captured from Run — used to verify pre-read behavior.
	runFileName     string
	runFileContent  string
	runGoal         string
	runContextFiles []string
}

func (m *mockAgent) Run(_ context.Context, fileName, fileContent, goal string, contextFiles []string) {
	m.runFileName = fileName
	m.runFileContent = fileContent
	m.runGoal = goal
	m.runContextFiles = contextFiles
	if m.runFunc != nil {
		go m.runFunc()
	}
}

func (m *mockAgent) Reply(_ string) bool {
	m.replied = true
	return true
}

func (m *mockAgent) Approve() {
	m.approved = true
}

func (m *mockAgent) Continue(path, content string) {
	m.continued = true
	m.lastPath = path
	m.lastContent = content
	if m.continueDone != nil {
		select {
		case m.continueDone <- struct{}{}:
		default:
		}
	}
}

func (m *mockAgent) Cancel() {
	m.cancelled = true
}

func (m *mockAgent) IsWaiting() bool {
	return false
}

func TestRunner_SimpleGoal_TokensAndDone(t *testing.T) {
	events := make(chan event.Event, 64)
	mock := &mockAgent{
		events: events,
		runFunc: func() {
			events <- event.AgentToken{Text: "Hello "}
			events <- event.AgentToken{Text: "world"}
			events <- event.AgentDone{Success: true}
		},
	}

	ws := NewDiskWorkspace(t.TempDir())
	var stderr bytes.Buffer
	runner := NewRunner(mock, ws, events, &stderr, false)

	result := runner.Run(context.Background(), "say hello", nil)

	if !result.Success {
		t.Error("expected success")
	}
	if result.Summary != "Hello world" {
		t.Errorf("summary = %q, want %q", result.Summary, "Hello world")
	}
}

func TestRunner_TTY_StreamsToStderr(t *testing.T) {
	events := make(chan event.Event, 64)
	mock := &mockAgent{
		events: events,
		runFunc: func() {
			events <- event.AgentToken{Text: "output text"}
			events <- event.AgentDone{Success: true}
		},
	}

	ws := NewDiskWorkspace(t.TempDir())
	var stderr bytes.Buffer
	runner := NewRunner(mock, ws, events, &stderr, true)

	runner.Run(context.Background(), "test", nil)

	if !strings.Contains(stderr.String(), "output text") {
		t.Errorf("stderr = %q, want to contain 'output text'", stderr.String())
	}
}

func TestRunner_EditProposed_AppliesAndContinues(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	// Create the file to edit.
	absPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(absPath, []byte("package main\n\nfunc old() {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := make(chan event.Event, 64)
	done := make(chan struct{}, 1)
	mock := &mockAgent{events: events, continueDone: done}
	mock.runFunc = func() {
		events <- event.AgentEditProposed{Edit: event.PendingEdit{
			ID:      "edit-1",
			Path:    "main.go",
			Search:  "func old() {}",
			Replace: "func new() {}",
		}}
		<-done // wait for applyEdit to call Continue
		events <- event.AgentDone{Success: true}
	}

	var stderr bytes.Buffer
	runner := NewRunner(mock, ws, events, &stderr, false)

	result := runner.Run(context.Background(), "rename function", nil)

	if !result.Success {
		t.Error("expected success")
	}
	if !mock.approved {
		t.Error("expected agent.Approve() to be called")
	}
	if !mock.continued {
		t.Error("expected agent.Continue() to be called")
	}

	// Verify the file was modified on disk.
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "func new() {}") {
		t.Errorf("file content = %q, want to contain 'func new() {}'", string(data))
	}
	if strings.Contains(string(data), "func old() {}") {
		t.Error("file still contains old function")
	}

	wantPath := filepath.Join(dir, "main.go")
	if len(result.FilesChanged) != 1 || result.FilesChanged[0] != wantPath {
		t.Errorf("FilesChanged = %v, want [%s]", result.FilesChanged, wantPath)
	}
}

func TestRunner_EditProposed_PreservesTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	// File with trailing newline — edit must preserve it on disk.
	absPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(absPath, []byte("package main\n\nfunc old() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := make(chan event.Event, 64)
	done := make(chan struct{}, 1)
	mock := &mockAgent{events: events, continueDone: done}
	mock.runFunc = func() {
		events <- event.AgentEditProposed{Edit: event.PendingEdit{
			ID:      "edit-nl",
			Path:    "main.go",
			Search:  "func old() {}",
			Replace: "func new() {}",
		}}
		<-done
		events <- event.AgentDone{Success: true}
	}

	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)
	result := runner.Run(context.Background(), "edit with newline", nil)

	if !result.Success {
		t.Error("expected success")
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "package main\n\nfunc new() {}\n"
	if got != want {
		t.Errorf("on-disk content = %q, want %q (trailing newline preserved)", got, want)
	}

	// Agent receives normalized content (no trailing \n), matching its view.
	if mock.lastContent != "package main\n\nfunc new() {}" {
		t.Errorf("agent received content = %q, want without trailing newline", mock.lastContent)
	}
}

func TestRunner_FileCreated_TrackedInResult(t *testing.T) {
	dir := t.TempDir()
	events := make(chan event.Event, 64)
	mock := &mockAgent{
		events: events,
		runFunc: func() {
			events <- event.AgentFileCreated{Path: "new_file.go"}
			events <- event.AgentDone{Success: true}
		},
	}

	ws := NewDiskWorkspace(dir)
	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)

	result := runner.Run(context.Background(), "create file", nil)

	wantPath := filepath.Join(dir, "new_file.go")
	if len(result.FilesCreated) != 1 || result.FilesCreated[0] != wantPath {
		t.Errorf("FilesCreated = %v, want [%s]", result.FilesCreated, wantPath)
	}
}

func TestRunner_AgentError_CollectedInResult(t *testing.T) {
	events := make(chan event.Event, 64)
	mock := &mockAgent{
		events: events,
		runFunc: func() {
			events <- event.AgentError{Err: "LLM timeout"}
			events <- event.AgentDone{Success: false}
		},
	}

	ws := NewDiskWorkspace(t.TempDir())
	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)

	result := runner.Run(context.Background(), "will fail", nil)

	if result.Success {
		t.Error("expected failure")
	}
	if len(result.Errors) != 1 || result.Errors[0] != "LLM timeout" {
		t.Errorf("Errors = %v, want [LLM timeout]", result.Errors)
	}
}

func TestRunner_FlushBuffers_RespondsImmediately(t *testing.T) {
	events := make(chan event.Event, 64)
	flushDone := make(chan bool, 1)
	mock := &mockAgent{events: events}
	mock.runFunc = func() {
		resultCh := make(chan event.FlushResult, 1)
		events <- event.FlushBuffers{Result: resultCh}
		res := <-resultCh
		if res.Err != nil {
			flushDone <- false
		} else {
			flushDone <- true
		}
		events <- event.AgentDone{Success: true}
	}

	ws := NewDiskWorkspace(t.TempDir())
	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)

	result := runner.Run(context.Background(), "flush test", nil)

	if !result.Success {
		t.Error("expected success")
	}
	select {
	case ok := <-flushDone:
		if !ok {
			t.Error("flush returned error")
		}
	case <-time.After(time.Second):
		t.Error("flush did not complete within timeout")
	}
}

func TestRunner_AgentWaiting_SingleShot_Exits(t *testing.T) {
	events := make(chan event.Event, 64)
	mock := &mockAgent{events: events}
	mock.runFunc = func() {
		events <- event.AgentToken{Text: "done thinking"}
		events <- event.AgentWaiting{}
		// Agent waits — in single-shot mode runner should cancel and exit.
	}

	ws := NewDiskWorkspace(t.TempDir())
	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false) // isTTY=false

	result := runner.Run(context.Background(), "one shot", nil)

	if !result.Success {
		t.Error("expected success in single-shot mode")
	}
	if !mock.cancelled {
		t.Error("expected agent.Cancel() to be called in single-shot mode")
	}
	if result.Summary != "done thinking" {
		t.Errorf("summary = %q, want %q", result.Summary, "done thinking")
	}
}

func TestRunner_ContextCancelled(t *testing.T) {
	events := make(chan event.Event, 64)
	ctx, cancel := context.WithCancel(context.Background())
	mock := &mockAgent{events: events}
	mock.runFunc = func() {
		// Cancel the context while agent is running.
		cancel()
	}

	ws := NewDiskWorkspace(t.TempDir())
	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)

	result := runner.Run(ctx, "will cancel", nil)

	if result.Success {
		t.Error("expected failure on context cancellation")
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0], "cancel") {
		t.Errorf("expected cancellation error, got %v", result.Errors)
	}
}

func TestRunner_PreReadsFirstFile(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)
	writeTestFile(t, dir, "target.go", "package target\n")

	events := make(chan event.Event, 64)
	mock := &mockAgent{events: events}
	mock.runFunc = func() {
		events <- event.AgentDone{Success: true}
	}

	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)
	result := runner.Run(context.Background(), "review", []string{"target.go", "other.go"})

	if !result.Success {
		t.Error("expected success")
	}
	if mock.runFileName != "target.go" {
		t.Errorf("agent received fileName = %q, want %q", mock.runFileName, "target.go")
	}
	// ReadFile trims trailing newline, matching buffer.NewFromFile behavior.
	if mock.runFileContent != "package target" {
		t.Errorf("agent received fileContent = %q, want %q", mock.runFileContent, "package target")
	}
	if mock.runGoal != "review" {
		t.Errorf("agent received goal = %q, want %q", mock.runGoal, "review")
	}
	if len(mock.runContextFiles) != 2 || mock.runContextFiles[0] != "target.go" || mock.runContextFiles[1] != "other.go" {
		t.Errorf("agent received contextFiles = %v, want [target.go other.go]", mock.runContextFiles)
	}
}

func TestRunner_EditSearchNotFound(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)
	writeTestFile(t, dir, "main.go", "package main")

	events := make(chan event.Event, 64)
	done := make(chan struct{}, 1)
	mock := &mockAgent{events: events, continueDone: done}
	mock.runFunc = func() {
		events <- event.AgentEditProposed{Edit: event.PendingEdit{
			ID:      "edit-1",
			Path:    "main.go",
			Search:  "nonexistent text",
			Replace: "replacement",
		}}
		<-done // wait for applyEdit to call Continue (even on error)
		events <- event.AgentDone{Success: true}
	}

	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)
	result := runner.Run(context.Background(), "bad edit", nil)

	// The agent reports success (it may try a different approach), but the
	// edit error is still recorded for the caller to inspect.
	if !result.Success {
		t.Error("expected success — agent succeeded despite the failed edit")
	}
	if len(result.Errors) == 0 {
		t.Error("expected error for search text not found")
	}
	if !strings.Contains(result.Errors[0], "not found") {
		t.Errorf("error %q does not mention 'not found'", result.Errors[0])
	}
}

func TestResult_WriteJSON(t *testing.T) {
	result := &Result{
		Success:      true,
		Summary:      "done",
		FilesChanged: []string{"b.go", "a.go"},
		FilesCreated: []string{"c.go"},
	}

	var buf bytes.Buffer
	if err := result.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}

	var decoded Result
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !decoded.Success {
		t.Error("decoded.Success = false, want true")
	}
	// Verify sorted.
	if len(decoded.FilesChanged) != 2 || decoded.FilesChanged[0] != "a.go" {
		t.Errorf("FilesChanged = %v, want [a.go, b.go] (sorted)", decoded.FilesChanged)
	}
}
