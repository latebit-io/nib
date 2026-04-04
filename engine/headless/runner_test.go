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
}

func (m *mockAgent) Run(_ context.Context, _, _, _ string, _ []string) {
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
	mock := &mockAgent{events: events}
	mock.runFunc = func() {
		events <- event.AgentEditProposed{Edit: event.PendingEdit{
			ID:      "edit-1",
			Path:    "main.go",
			Search:  "func old() {}",
			Replace: "func new() {}",
		}}
		// Give the runner time to process the edit before sending done.
		time.Sleep(50 * time.Millisecond)
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

	if len(result.FilesChanged) != 1 || result.FilesChanged[0] != "main.go" {
		t.Errorf("FilesChanged = %v, want [main.go]", result.FilesChanged)
	}
}

func TestRunner_FileCreated_TrackedInResult(t *testing.T) {
	events := make(chan event.Event, 64)
	mock := &mockAgent{
		events: events,
		runFunc: func() {
			events <- event.AgentFileCreated{Path: "new_file.go"}
			events <- event.AgentDone{Success: true}
		},
	}

	ws := NewDiskWorkspace(t.TempDir())
	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)

	result := runner.Run(context.Background(), "create file", nil)

	if len(result.FilesCreated) != 1 || result.FilesCreated[0] != "new_file.go" {
		t.Errorf("FilesCreated = %v, want [new_file.go]", result.FilesCreated)
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
	result := runner.Run(context.Background(), "review", []string{"target.go"})

	if !result.Success {
		t.Error("expected success")
	}
}

func TestRunner_EditSearchNotFound(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)
	writeTestFile(t, dir, "main.go", "package main")

	events := make(chan event.Event, 64)
	mock := &mockAgent{events: events}
	mock.runFunc = func() {
		events <- event.AgentEditProposed{Edit: event.PendingEdit{
			ID:      "edit-1",
			Path:    "main.go",
			Search:  "nonexistent text",
			Replace: "replacement",
		}}
		time.Sleep(50 * time.Millisecond)
		events <- event.AgentDone{Success: true}
	}

	runner := NewRunner(mock, ws, events, &bytes.Buffer{}, false)
	result := runner.Run(context.Background(), "bad edit", nil)

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
