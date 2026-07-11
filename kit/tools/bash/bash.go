// Package bash provides a kit-generic shell-execution tool. The tool
// runs commands inside a project root with a head+tail output cap,
// timeout enforcement, and a guard layer (see [guard.go]) that blocks
// destructive or write-bypassing patterns at the shell layer rather
// than relying solely on system-prompt prose.
//
// The tool depends only on [github.com/latebit-io/nib/agent] for the
// generic Tool/ToolResult types and [github.com/latebit-io/nib/ai/llm]
// for the tool definition shape — no kit, no coding, no engine
// imports. Any kit-based agent can register it.
package bash

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// defaultBashTimeout is the maximum duration a bash command can run
// when the caller does not supply a timeout argument.
const defaultBashTimeout = 30 * time.Second

// maxBashTimeout is the absolute maximum timeout the agent can request.
const maxBashTimeout = 120 * time.Second

// maxBashHead is the byte budget for the beginning of command output.
// Captures initial context (command echo, early output).
const maxBashHead = 4 * 1024

// maxBashTail is the byte budget for the end of command output.
// Captures the most recent output (error messages, test failures).
const maxBashTail = 4 * 1024

// Tool lets the LLM execute shell commands in a project directory.
//
// Trust model: commands come from the LLM, which is instructed via
// the system prompt not to run destructive operations. This is
// prompt-level guidance, not enforcement; the guards in [guard.go]
// supplement the prompt with deterministic blocks for the highest-
// risk shapes (in-place edits, recursive deletes, search-tool
// invocations, history-rewriting git ops). The developer can cancel
// the agent at any time, and the timeout prevents runaway processes.
type Tool struct {
	projectRoot string
	// approvalManaged relaxes the file-write and destructive guard
	// classes from hard blocks to pass-through, because an external
	// per-command approval surface already gated the command before
	// Execute ran (see [ApprovalManaged]). The search guard stays hard
	// regardless — it redirects to better tools, which approval cannot
	// improve on.
	approvalManaged bool
}

// Option configures a [Tool] at construction.
type Option func(*Tool)

// ApprovalManaged marks the tool as wrapped by a per-command approval
// surface: the file-write and destructive guard classes no longer hard-
// block inside Execute, because the wrapper proposes those commands to
// the developer (with the guard's classification as the reason) and only
// executes on explicit approval. Without this option those classes
// hard-block as always. Do NOT set it unless every command genuinely
// passes through an approval gate first.
func ApprovalManaged() Option {
	return func(t *Tool) { t.approvalManaged = true }
}

// New creates a [Tool] rooted at the given project directory.
func New(projectRoot string, opts ...Option) *Tool {
	t := &Tool{projectRoot: projectRoot}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Definition returns the OpenAI-compatible tool schema for bash.
func (t *Tool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "bash",
			Description: "Execute a shell command in the project directory. " +
				"Use this to verify edits compile (go build ./...), run tests (go test ./...), " +
				"check formatting, or explore the project. " +
				"Do NOT use for destructive operations (rm -rf, git push) unless the developer explicitly asked.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"command": {
						Type:        "string",
						Description: "The shell command to execute (e.g. \"go build ./...\").",
					},
					"timeout": {
						Type:        "integer",
						Description: "Optional timeout in seconds (default 30, max 120).",
					},
				},
				Required: []string{"command"},
			},
		},
	}
}

type bashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Execute runs a shell command and returns the combined output and exit code.
func (t *Tool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	var args bashArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Command == "" {
		return errorResult("Error: command is required")
	}

	if class, msg := Classify(args.Command); msg != "" && (class == GuardSearch || !t.approvalManaged) {
		// Approval-managed mode passes file-write and destructive
		// classes through — the wrapping gate already proposed them and
		// the developer approved. The search class blocks regardless
		// (redirect to structured tools, not a risk decision).
		//
		// Log the guard classification (msg) and a redacted preview
		// instead of the full command — the verbatim command may carry
		// secrets that should not land in slog. The LLM still sees the
		// full guard message via the tool result, so debugging is not
		// degraded.
		slog.Warn("bash: blocked by guard",
			"reason", firstLine(msg),
			"preview", redactCommandPreview(args.Command))
		return errorResult(msg)
	}

	timeout := defaultBashTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	slog.Debug("bash: executing", "command", args.Command, "timeout", timeout)

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", args.Command)
	cmd.Dir = t.projectRoot

	// Run in its own process group so we can kill all children (not just
	// the shell) when the context deadline fires. Without this, commands
	// like `go run .` spawn child processes that outlive the shell and
	// hold stdout/stderr pipes open, blocking cmd.Run() indefinitely.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Grace period for pipe drain after process exit. Prevents cmd.Run()
	// from hanging if a child process still holds a pipe fd.
	cmd.WaitDelay = time.Second

	// Capture head and tail of output to preserve both context and errors.
	// Prevents OOM from high-volume output while keeping the useful parts.
	htw := newHeadTailWriter(maxBashHead, maxBashTail)
	cmd.Stdout = htw
	cmd.Stderr = htw

	err := cmd.Run()

	output := htw.String()

	exitCode := 0
	if err != nil {
		// Check timeout before exit code — CommandContext kills the process
		// on deadline, which produces an ExitError with code -1.
		if cmdCtx.Err() == context.DeadlineExceeded {
			// Log the redacted preview, not the verbatim command — the
			// timeout path runs at Warn level and an inline secret
			// (e.g. `MY_TOKEN=xyz ./script.sh`) would otherwise land
			// in slog. Mirrors the guard path's treatment at the top
			// of Execute.
			slog.Warn("bash: command timed out",
				"preview", redactCommandPreview(args.Command),
				"timeout", timeout)
			return errorResult(fmt.Sprintf("Error: command timed out after %s\n\n%s", timeout, output))
		}
		if ctx.Err() != nil {
			return errorResult("Error: cancelled")
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return errorResult(fmt.Sprintf("Error: %v\n\n%s", err, output))
		}
	}

	slog.Debug("bash: completed", "command", args.Command, "exit_code", exitCode, "output_len", len(output))

	if exitCode != 0 {
		return errorResult(fmt.Sprintf("Exit code: %d\n\n%s", exitCode, output))
	}
	if output == "" {
		return textResult("(no output)")
	}
	return textResult(output)
}

func textResult(content string) agent.ToolResult {
	return agent.ToolResult{Content: content}
}

func errorResult(content string) agent.ToolResult {
	return agent.ToolResult{Content: content, IsError: true}
}

// headTailWriter captures the first headSize bytes and the last tailSize bytes
// of a stream, discarding the middle. This preserves both the initial context
// (command echo, early output) and the tail (error messages, test failures)
// while bounding memory during execution.
//
// When total output fits within headSize, no truncation occurs and the tail
// buffer is unused. Once head fills, subsequent writes go into a circular
// ring buffer that always retains the most recent tailSize bytes.
type headTailWriter struct {
	mu       sync.Mutex
	head     []byte
	headCap  int
	tail     []byte // circular ring buffer
	tailCap  int
	tailPos  int // next write position in ring
	tailFull bool
	total    int // total bytes written (for collapse message)
}

// newHeadTailWriter creates a writer that keeps the first headSize bytes
// and the last tailSize bytes of output.
func newHeadTailWriter(headSize, tailSize int) *headTailWriter {
	return &headTailWriter{
		head:    make([]byte, 0, headSize),
		headCap: headSize,
		tail:    make([]byte, tailSize),
		tailCap: tailSize,
	}
}

// Write implements io.Writer. Always returns len(p), nil so the subprocess
// never stalls on a blocked pipe. Safe for concurrent use (stdout + stderr
// are drained by separate goroutines in os/exec).
func (w *headTailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(p)
	w.total += n

	// Fill head first.
	if len(w.head) < w.headCap {
		room := w.headCap - len(w.head)
		if room >= len(p) {
			w.head = append(w.head, p...)
			return n, nil
		}
		w.head = append(w.head, p[:room]...)
		p = p[room:]
	}

	// Remainder goes into the circular tail buffer.
	for len(p) > 0 {
		chunk := w.tailCap - w.tailPos
		if chunk > len(p) {
			chunk = len(p)
		}
		copy(w.tail[w.tailPos:w.tailPos+chunk], p[:chunk])
		w.tailPos += chunk
		if w.tailPos >= w.tailCap {
			w.tailPos = 0
			w.tailFull = true
		}
		p = p[chunk:]
	}

	return n, nil
}

// String returns the captured output. If no truncation occurred, returns
// the head buffer only. Otherwise returns head + collapse marker + tail.
// Must be called after cmd.Run() returns (no concurrent writes).
func (w *headTailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	headStr := string(w.head)

	// No overflow — everything fit in head.
	if w.total <= w.headCap {
		return headStr
	}

	// Reconstruct tail from the circular buffer.
	var tailStr string
	if w.tailFull {
		// Ring wrapped: data is [tailPos..end] + [0..tailPos].
		tailStr = string(w.tail[w.tailPos:]) + string(w.tail[:w.tailPos])
	} else {
		tailStr = string(w.tail[:w.tailPos])
	}

	dropped := w.total - len(w.head) - len(tailStr)
	if dropped <= 0 {
		// Everything fit in head + tail — no middle was lost.
		return headStr + tailStr
	}
	return fmt.Sprintf("%s\n\n[... %d bytes collapsed — showing first %d and last %d bytes ...]\n\n%s",
		headStr, dropped, len(w.head), len(tailStr), tailStr)
}

// PromptGuidelines returns the usage bullets this tool contributes to a
// consumer's system prompt (kit.PromptContributor, satisfied
// structurally — this package cannot import kit).
func (t *Tool) PromptGuidelines() []string {
	if t.approvalManaged {
		return []string{
			"`bash` is for build/test commands — not for editing files, not for searching code. " +
				"Destructive or file-writing commands are proposed to the developer for approval before they run; " +
				"prefer the edit tools and only propose such commands when the task genuinely needs them.",
		}
	}
	return []string{
		"`bash` is for build/test commands — not for editing files, not for searching code, not for destructive ops without explicit developer ask.",
	}
}
