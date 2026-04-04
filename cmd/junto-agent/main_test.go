package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/headless"
	"github.com/latebit-io/junto/engine/llm"
)

// mockProvider returns pre-configured streaming responses for each turn.
type mockProvider struct {
	turns [][]llm.StreamEvent
	call  int
}

func (m *mockProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	if m.call >= len(m.turns) {
		return nil, fmt.Errorf("unexpected Stream call %d", m.call+1)
	}
	events := m.turns[m.call]
	m.call++
	ch := make(chan llm.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func TestEndToEnd_SimpleGoal_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	ws := headless.NewDiskWorkspace(dir)
	events := make(chan event.Event, 128)

	provider := &mockProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "I reviewed the code."},
				{Token: " Everything looks good.", Done: true},
			},
		},
	}

	ag := agent.New(provider, ws, events, &agent.NewOptions{
		Interaction: agent.Headless,
	})

	runner := headless.NewRunner(ag, ws, events, &bytes.Buffer{}, false)
	result := runner.Run(context.Background(), "review the code", nil)

	if !result.Success {
		t.Fatalf("expected success, got errors: %v", result.Errors)
	}
	if result.Summary == "" {
		t.Error("expected non-empty summary")
	}

	// Verify JSON serialization round-trips correctly.
	var buf bytes.Buffer
	if err := result.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}

	var decoded headless.Result
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, buf.String())
	}
	if !decoded.Success {
		t.Error("decoded success = false")
	}
	if decoded.Summary == "" {
		t.Error("decoded summary should be non-empty")
	}
}

func TestEndToEnd_EditFile_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	ws := headless.NewDiskWorkspace(dir)

	// Create a file for the agent to edit.
	targetPath := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(targetPath, []byte("package main\n\nfunc hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := make(chan event.Event, 128)

	// Turn 1: agent reads the file via tool call.
	// Turn 2: agent edits the file via tool call.
	// Turn 3: agent responds with summary (no tool calls).
	provider := &mockProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: read_file tool call.
			{
				{ToolCalls: []llm.ToolCall{{
					ID:   "call-1",
					Type: "function",
					Function: llm.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path": "hello.go"}`,
					},
				}}, Done: true},
			},
			// Turn 2: edit_file tool call.
			{
				{ToolCalls: []llm.ToolCall{{
					ID:   "call-2",
					Type: "function",
					Function: llm.FunctionCall{
						Name:      "edit_file",
						Arguments: `{"path": "hello.go", "search": "func hello() {}", "replace": "func hello() {\n\tfmt.Println(\"hello\")\n}", "reason": "add print"}`,
					},
				}}, Done: true},
			},
			// Turn 3: final text response.
			{
				{Token: "Added a print statement.", Done: true},
			},
		},
	}

	ag := agent.New(provider, ws, events, &agent.NewOptions{
		Interaction: agent.Headless,
	})

	runner := headless.NewRunner(ag, ws, events, &bytes.Buffer{}, false)
	result := runner.Run(context.Background(), "add a print to hello", []string{"hello.go"})

	if !result.Success {
		t.Fatalf("expected success, got errors: %v", result.Errors)
	}

	// Verify the file was edited on disk.
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if content == "package main\n\nfunc hello() {}\n" {
		t.Error("file was not modified")
	}
	if !bytes.Contains(data, []byte("Println")) {
		t.Errorf("file does not contain expected edit, got:\n%s", content)
	}

	// Verify JSON output includes changed files.
	var buf bytes.Buffer
	if err := result.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}

	var decoded headless.Result
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, buf.String())
	}
	if len(decoded.FilesChanged) == 0 {
		t.Error("expected FilesChanged to be non-empty")
	}
}

func TestEndToEnd_WriteText_HumanOutput(t *testing.T) {
	result := &headless.Result{
		Success:      true,
		Summary:      "Done.",
		FilesChanged: []string{"/tmp/a.go"},
		FilesCreated: []string{"/tmp/b.go"},
	}

	err := writeText(result)
	if err != nil {
		t.Errorf("writeText returned error for successful result: %v", err)
	}
}

func TestEndToEnd_WriteText_ErrorResult(t *testing.T) {
	result := &headless.Result{
		Success: false,
		Summary: "Failed.",
		Errors:  []string{"something broke"},
	}

	err := writeText(result)
	if err == nil {
		t.Error("writeText should return error for failed result")
	}
}

func TestResolveProjectRoot_Override(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveProjectRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("resolveProjectRoot(%q) = %q, want %q", dir, got, dir)
	}
}

func TestResolveProjectRoot_NotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveProjectRoot(f)
	if err == nil {
		t.Error("expected error for non-directory path")
	}
}
