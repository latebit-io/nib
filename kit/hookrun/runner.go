package hookrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/latebit-io/nib/kit/hookspec"
)

const (
	// defaultTimeout bounds a hook command when neither the hook nor the
	// runner specifies one.
	defaultTimeout = 30 * time.Second
	// maxHookOutput caps captured stdout/stderr. A hook's reason is fed
	// back into the agent, so it needs an explicit upper bound.
	maxHookOutput = 32 << 10 // 32 KiB
)

// Decision is the outcome a hook reports for the action that triggered it.
type Decision string

const (
	// Proceed means continue normally (a clean run with no objection).
	Proceed Decision = "proceed"
	// Deny means block the action that triggered the hook.
	Deny Decision = "deny"
)

// Result is the outcome of running one hook command.
type Result struct {
	// Decision is Deny when the hook objects, else Proceed.
	Decision Decision
	// Reason is the hook's feedback (surfaced to the agent on Deny).
	Reason string
	// Output is the captured combined stdout+stderr (capped).
	Output string
	// ExitCode is the process exit code (-1 if it never ran).
	ExitCode int
	// Err is set on an infrastructure failure (spawn error, timeout). On
	// such a failure the runner FAILS OPEN (Decision=Proceed): a broken
	// hook must not wedge all work, though a clean non-zero exit IS a Deny.
	Err error
}

// Runner executes command hooks. The zero value is usable (default
// timeout); set Timeout to override.
type Runner struct {
	// Timeout is the per-hook cap; <=0 uses [defaultTimeout]. A hook's own
	// TimeoutMS, when set, takes precedence over this.
	Timeout time.Duration
}

// Run executes a command hook with the given input on stdin and returns
// its decision. A non-command/empty hook is a no-op Proceed.
//
// Decision resolution: if stdout is JSON with a "decision" field, that
// wins ("deny"/"block" → Deny, else Proceed). Otherwise the exit code
// decides — zero proceeds, non-zero denies (CC's convention), with the
// captured output as the reason.
func (r Runner) Run(ctx context.Context, h hookspec.Hook, in Input) Result {
	if !h.Runnable() {
		return Result{Decision: Proceed, ExitCode: -1}
	}
	payload, err := in.encode()
	if err != nil {
		return Result{Decision: Proceed, ExitCode: -1, Err: fmt.Errorf("hookrun: encode input: %w", err)}
	}

	timeout := r.Timeout
	if h.TimeoutMS > 0 {
		timeout = time.Duration(h.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := buildCmd(ctx, h.Command)
	if len(h.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range h.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = bytes.NewReader(payload)
	out := &capBuffer{limit: maxHookOutput}
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()
	output := out.String()

	// Infrastructure failures fail OPEN (a broken/slow hook must not wedge
	// all work): a timeout/cancel, or a process that never started.
	if runErr != nil {
		if ctx.Err() != nil {
			return Result{Decision: Proceed, ExitCode: -1, Output: output, Err: fmt.Errorf("hookrun: hook timed out: %w", ctx.Err())}
		}
		if cmd.ProcessState == nil {
			return Result{Decision: Proceed, ExitCode: -1, Output: output, Err: fmt.Errorf("hookrun: run hook: %w", runErr)}
		}
		// Otherwise the hook ran and exited non-zero — a real Deny, handled
		// by the exit-code logic below.
	}
	exit := cmd.ProcessState.ExitCode()

	if dec, reason, ok := parseDecision(output); ok {
		return Result{Decision: dec, Reason: reason, Output: output, ExitCode: exit}
	}
	// No JSON decision: exit code decides.
	if exit != 0 {
		return Result{Decision: Deny, Reason: strings.TrimSpace(output), Output: output, ExitCode: exit}
	}
	return Result{Decision: Proceed, Output: output, ExitCode: exit}
}

// buildCmd builds the exec command. A single-element command is run via
// `sh -c` (CC's shell-string form, supporting pipes/quotes); a multi-
// element command is run argv-style (no shell). Hooks run only for
// trusted plugins, so shell execution is acceptable here.
func buildCmd(ctx context.Context, command []string) *exec.Cmd {
	if len(command) == 1 {
		return exec.CommandContext(ctx, "sh", "-c", command[0])
	}
	return exec.CommandContext(ctx, command[0], command[1:]...)
}

// hookOutput is the recognized JSON decision shape on a hook's stdout.
type hookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// parseDecision tries to read a JSON decision from output. ok is false
// when output is not a JSON object with a decision, so the caller falls
// back to exit-code semantics.
func parseDecision(output string) (dec Decision, reason string, ok bool) {
	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, "{") {
		return "", "", false
	}
	var ho hookOutput
	if err := json.Unmarshal([]byte(trimmed), &ho); err != nil || ho.Decision == "" {
		return "", "", false
	}
	switch strings.ToLower(ho.Decision) {
	case "deny", "block":
		return Deny, ho.Reason, true
	default:
		return Proceed, ho.Reason, true
	}
}

// capBuffer accumulates up to limit bytes and discards the rest, bounding
// the memory and prompt cost of a noisy hook.
type capBuffer struct {
	b         bytes.Buffer
	limit     int
	truncated bool
}

func (w *capBuffer) Write(p []byte) (int, error) {
	if rem := w.limit - w.b.Len(); rem > 0 {
		if len(p) <= rem {
			w.b.Write(p)
		} else {
			w.b.Write(p[:rem])
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *capBuffer) String() string {
	if w.truncated {
		return w.b.String() + fmt.Sprintf("\n[output truncated at %d bytes]", w.limit)
	}
	return w.b.String()
}
