package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

func bashCall(command string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{"command": command})
	return llm.ToolCall{
		ID: "test-1",
		Function: llm.FunctionCall{
			Name:      "bash",
			Arguments: string(args),
		},
	}
}

func TestBashTool_SimpleCommand(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), bashCall("echo hello"))
	if !strings.Contains(result.Content, "hello") {
		t.Errorf("expected output to contain 'hello', got %q", result.Content)
	}
}

func TestBashTool_ExitCode(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), bashCall("exit 1"))
	if !strings.Contains(result.Content, "Exit code: 1") {
		t.Errorf("expected exit code 1, got %q", result.Content)
	}
}

func TestBashTool_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), bashCall("pwd"))
	if !strings.Contains(result.Content, dir) {
		t.Errorf("expected working directory %q in output, got %q", dir, result.Content)
	}
}

func TestBashTool_EmptyCommand(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), llm.ToolCall{
		ID: "test-1",
		Function: llm.FunctionCall{
			Name:      "bash",
			Arguments: `{"command":""}`,
		},
	})
	if !strings.Contains(result.Content, "Error: command is required") {
		t.Errorf("expected error for empty command, got %q", result.Content)
	}
}

func TestBashTool_InvalidArgs(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), llm.ToolCall{
		ID: "test-1",
		Function: llm.FunctionCall{
			Name:      "bash",
			Arguments: `{invalid`,
		},
	})
	if !strings.Contains(result.Content, "Error: invalid arguments") {
		t.Errorf("expected invalid arguments error, got %q", result.Content)
	}
}

func TestBashTool_OutputTruncation(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	// Generate output larger than maxBashOutput (8KB).
	// Use yes piped to head for a portable large output.
	result := tool.Execute(context.Background(), bashCall("yes | head -c 16384"))
	if !strings.Contains(result.Content, "[... output truncated]") {
		t.Errorf("expected truncation marker, got length %d", len(result.Content))
	}
}

func TestBashTool_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := tool.Execute(ctx, bashCall("sleep 10"))
	if !strings.Contains(result.Content, "Error") {
		t.Errorf("expected error on cancelled context, got %q", result.Content)
	}
}

func TestBashTool_Timeout(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	call := llm.ToolCall{
		ID: "test-1",
		Function: llm.FunctionCall{
			Name:      "bash",
			Arguments: `{"command":"sleep 10","timeout":1}`,
		},
	}
	result := tool.Execute(context.Background(), call)
	if !strings.Contains(result.Content, "timed out") {
		t.Errorf("expected timeout error, got %q", result.Content)
	}
}

func TestBashTool_NoOutput(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), bashCall("true"))
	if result.Content != "(no output)" {
		t.Errorf("expected '(no output)', got %q", result.Content)
	}
}

func TestBashTool_StderrCaptured(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)

	result := tool.Execute(context.Background(), bashCall("echo error >&2"))
	if !strings.Contains(result.Content, "error") {
		t.Errorf("expected stderr captured, got %q", result.Content)
	}
}

func TestBashTool_Definition(t *testing.T) {
	tool := NewBashTool("/tmp")
	def := tool.Definition()
	if def.Function.Name != "bash" {
		t.Errorf("expected tool name 'bash', got %q", def.Function.Name)
	}
	if _, ok := def.Function.Parameters.Properties["command"]; !ok {
		t.Error("expected 'command' parameter in definition")
	}
}
