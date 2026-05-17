package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
)

func bashCall(command string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{"command": command}) // marshal of map[string]string cannot fail
	return llm.ToolCall{
		ID: "test-1",
		Function: llm.FunctionCall{
			Name:      "bash",
			Arguments: string(args),
		},
	}
}

func TestTool_SimpleCommand(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	result := tool.Execute(context.Background(), bashCall("echo hello"))
	if !strings.Contains(result.Content, "hello") {
		t.Errorf("expected output to contain 'hello', got %q", result.Content)
	}
}

func TestTool_ExitCode(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	result := tool.Execute(context.Background(), bashCall("exit 1"))
	if !strings.Contains(result.Content, "Exit code: 1") {
		t.Errorf("expected exit code 1, got %q", result.Content)
	}
}

func TestTool_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	result := tool.Execute(context.Background(), bashCall("pwd"))
	if !strings.Contains(result.Content, dir) {
		t.Errorf("expected working directory %q in output, got %q", dir, result.Content)
	}
}

func TestTool_EmptyCommand(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

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

func TestTool_InvalidArgs(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

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

func TestTool_OutputTruncation(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	// Generate output larger than head+tail budget (~50 KiB total).
	// 100 KiB blows past the cap and forces the collapse path.
	result := tool.Execute(context.Background(), bashCall("yes | head -c 102400"))
	if !strings.Contains(result.Content, "bytes collapsed") {
		t.Errorf("expected collapse marker, got length %d:\n%s", len(result.Content), result.Content)
	}
	// Body must stay near the head+tail cap regardless of input size.
	if len(result.Content) > maxBashHead+maxBashTail+1024 {
		t.Errorf("result %d bytes exceeds head+tail+marker budget %d", len(result.Content), maxBashHead+maxBashTail+1024)
	}
}

func TestTool_TailPreservation(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	// Emit a large block then a known sentinel at the end. The sentinel
	// must survive in the tail even though the middle is collapsed.
	cmd := "yes | head -c 102400; echo SENTINEL_TAIL_MARKER"
	result := tool.Execute(context.Background(), bashCall(cmd))

	if !strings.Contains(result.Content, "SENTINEL_TAIL_MARKER") {
		t.Errorf("expected tail to contain sentinel, got:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "bytes collapsed") {
		t.Errorf("expected collapse marker, got:\n%s", result.Content)
	}
}

func TestTool_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	result := tool.Execute(ctx, bashCall("sleep 10"))
	if !strings.Contains(result.Content, "Error") {
		t.Errorf("expected error on cancelled context, got %q", result.Content)
	}
}

func TestTool_Timeout(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

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

func TestTool_NoOutput(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	result := tool.Execute(context.Background(), bashCall("true"))
	if result.Content != "(no output)" {
		t.Errorf("expected '(no output)', got %q", result.Content)
	}
}

func TestTool_StderrCaptured(t *testing.T) {
	dir := t.TempDir()
	tool := New(dir)

	result := tool.Execute(context.Background(), bashCall("echo error >&2"))
	if !strings.Contains(result.Content, "error") {
		t.Errorf("expected stderr captured, got %q", result.Content)
	}
}

func TestTool_Definition(t *testing.T) {
	tool := New("/tmp")
	def := tool.Definition()
	if def.Function.Name != "bash" {
		t.Errorf("expected tool name 'bash', got %q", def.Function.Name)
	}
	if _, ok := def.Function.Parameters.Properties["command"]; !ok {
		t.Error("expected 'command' parameter in definition")
	}
}

// writeStr is a test helper that writes a string to a headTailWriter.
func writeStr(t *testing.T, w *headTailWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
}

func TestHeadTailWriter_SmallOutput(t *testing.T) {
	w := newHeadTailWriter(100, 100)
	writeStr(t, w, "hello world")
	got := w.String()
	if got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}

func TestHeadTailWriter_ExactHeadFit(t *testing.T) {
	w := newHeadTailWriter(5, 5)
	writeStr(t, w, "12345")
	got := w.String()
	if got != "12345" {
		t.Errorf("expected '12345', got %q", got)
	}
}

func TestHeadTailWriter_ExactFit(t *testing.T) {
	w := newHeadTailWriter(4, 4)
	writeStr(t, w, "HEADTAIL") // exactly head + tail capacity
	got := w.String()

	if strings.Contains(got, "collapsed") {
		t.Errorf("should not show collapse message when nothing dropped, got %q", got)
	}
	if got != "HEADTAIL" {
		t.Errorf("expected 'HEADTAIL', got %q", got)
	}
}

func TestHeadTailWriter_HeadAndTail(t *testing.T) {
	w := newHeadTailWriter(4, 4)
	// Write 12 bytes: head gets "HEAD", tail ring gets last 4 of "MIDDTAIL" = "TAIL"
	writeStr(t, w, "HEADMIDDTAIL")
	got := w.String()

	if !strings.HasPrefix(got, "HEAD") {
		t.Errorf("expected head 'HEAD', got %q", got)
	}
	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("expected tail 'TAIL', got %q", got)
	}
	if !strings.Contains(got, "4 bytes collapsed") {
		t.Errorf("expected collapse message, got %q", got)
	}
}

func TestHeadTailWriter_RingWrap(t *testing.T) {
	w := newHeadTailWriter(2, 4)
	// Write in small chunks to exercise ring wrapping.
	writeStr(t, w, "AB")   // fills head
	writeStr(t, w, "CDEF") // fills tail [C,D,E,F], pos=0, full=true
	writeStr(t, w, "GH")   // overwrites [G,H,E,F], pos=2
	got := w.String()

	if !strings.HasPrefix(got, "AB") {
		t.Errorf("expected head 'AB', got %q", got)
	}
	// Tail should be "EFGH" (ring: [G,H,E,F] read from pos 2)
	if !strings.HasSuffix(got, "EFGH") {
		t.Errorf("expected tail 'EFGH', got %q", got)
	}
}

func TestHeadTailWriter_TotalTracking(t *testing.T) {
	w := newHeadTailWriter(4, 4)
	writeStr(t, w, "123456789012") // 12 bytes
	got := w.String()

	// Head: "1234", tail: "9012", dropped: 4
	if !strings.Contains(got, "4 bytes collapsed") {
		t.Errorf("expected 4 bytes collapsed, got %q", got)
	}
}

// TestTool_SatisfiesContract verifies the bash tool conforms to the
// [kit.Tool] contract.
func TestTool_SatisfiesContract(t *testing.T) {
	contracttest.Tool(t, func() kit.Tool { return New(t.TempDir()) })
}
