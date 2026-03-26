package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"time"

	"github.com/latebit-io/junto/engine/llm"
)

// defaultBashTimeout is the maximum duration a bash command can run.
const defaultBashTimeout = 30 * time.Second

// maxBashTimeout is the absolute maximum timeout the agent can request.
const maxBashTimeout = 120 * time.Second

// maxBashOutput caps the combined stdout+stderr returned to the LLM.
const maxBashOutput = 8 * 1024

// BashTool lets the LLM execute shell commands in the project directory.
//
// Trust model: commands come from the LLM, which is instructed via the system
// prompt not to run destructive operations. This is prompt-level guidance, not
// enforcement. The developer can cancel the agent at any time (Esc), and the
// timeout prevents runaway processes. Per-command approval and sandboxing are
// planned follow-ups — for now, the developer controls scope via intent and
// context set, same as with edit_file.
type BashTool struct {
	projectRoot string
}

// NewBashTool creates a BashTool rooted at the given project directory.
func NewBashTool(projectRoot string) *BashTool {
	return &BashTool{projectRoot: projectRoot}
}

// Definition returns the OpenAI-compatible tool schema for bash.
func (t *BashTool) Definition() llm.ToolDef {
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
func (t *BashTool) Execute(ctx context.Context, call llm.ToolCall) string {
	var args bashArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Command == "" {
		return "Error: command is required"
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

	// Cap buffer during execution to prevent OOM from high-volume output.
	// LimitedWriter stops accepting writes after maxBashOutput bytes.
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, remaining: maxBashOutput}
	cmd.Stdout = lw
	cmd.Stderr = lw

	err := cmd.Run()

	output := buf.String()
	if lw.truncated {
		output += "\n[... output truncated]"
	}

	exitCode := 0
	if err != nil {
		// Check timeout before exit code — CommandContext kills the process
		// on deadline, which produces an ExitError with code -1.
		if cmdCtx.Err() == context.DeadlineExceeded {
			slog.Warn("bash: command timed out", "command", args.Command, "timeout", timeout)
			return fmt.Sprintf("Error: command timed out after %s\n\n%s", timeout, output)
		}
		if ctx.Err() != nil {
			return "Error: cancelled"
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return fmt.Sprintf("Error: %v\n\n%s", err, output)
		}
	}

	slog.Debug("bash: completed", "command", args.Command, "exit_code", exitCode, "output_len", len(output))

	if exitCode != 0 {
		return fmt.Sprintf("Exit code: %d\n\n%s", exitCode, output)
	}
	if output == "" {
		return "(no output)"
	}
	return output
}

// limitedWriter caps writes at a byte limit, discarding excess.
// This prevents OOM from commands that produce unbounded output
// (e.g. `yes` or verbose test runs) during the execution itself,
// rather than only truncating after the command completes.
type limitedWriter struct {
	w         io.Writer
	remaining int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.remaining <= 0 {
		lw.truncated = true
		return len(p), nil // discard but report success so the process doesn't stall
	}
	if len(p) > lw.remaining {
		lw.truncated = true
		n, err := lw.w.Write(p[:lw.remaining])
		lw.remaining = 0
		if err != nil {
			return n, err
		}
		return len(p), nil // report full write to caller
	}
	n, err := lw.w.Write(p)
	lw.remaining -= n
	return n, err
}
