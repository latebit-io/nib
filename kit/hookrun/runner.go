package hookrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/nib/kit/hookspec"
)

const (
	// defaultTimeout bounds a hook command when neither the hook nor the
	// runner specifies one.
	defaultTimeout = 60 * time.Second
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
	// Allow means the hook approved a tool call outright (PreToolUse).
	Allow Decision = "allow"
	// Ask means the hook wants the person asked about a tool call.
	Ask Decision = "ask"
)

// Result is the outcome of running one hook command, read the way
// Claude Code reads it.
type Result struct {
	// Decision is Deny when the hook blocks, else what it decided.
	Decision Decision
	// Reason is the hook's explanation (surfaced to the agent on Deny).
	Reason string
	// Context is text the hook adds for the model: additionalContext,
	// or plain stdout from a clean run.
	Context string
	// Stop is set when the hook asks to end the session's work
	// ("continue": false), with StopReason to show.
	Stop       bool
	StopReason string
	// Output is the captured combined stdout+stderr (capped).
	Output string
	// ExitCode is the process exit code (-1 if it never ran).
	ExitCode int
	// Err is set when the hook failed: a spawn error, a timeout, or an
	// exit code other than 0 and 2. Such a failure FAILS OPEN
	// (Decision=Proceed): a broken hook must not wedge all work.
	Err error
}

// Runner executes command hooks. The zero value is usable (default
// timeout, the process's directory and environment).
type Runner struct {
	// Timeout is the per-hook cap; <=0 uses [defaultTimeout]. A hook's own
	// Timeout, when set, takes precedence over this.
	Timeout time.Duration
	// Dir is where hooks run; empty means the process's directory.
	Dir string
	// Env is added to every hook's environment, before the hook's own.
	Env map[string]string
}

// Run executes a command hook with in as JSON on stdin. As in Claude
// Code, exit 2 blocks (stderr is the reason), exit 0 proceeds and may
// print a JSON decision, any other exit is an error that does not block.
func (r Runner) Run(ctx context.Context, h hookspec.Hook, in Input) Result {
	if !h.Runnable() {
		return Result{Decision: Proceed, ExitCode: -1}
	}
	payload, err := in.encode()
	if err != nil {
		return Result{Decision: Proceed, ExitCode: -1, Err: fmt.Errorf("hookrun: encode input: %w", err)}
	}

	timeout := r.Timeout
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := buildCmd(ctx, h)
	cmd.Dir = r.Dir
	if len(r.Env) > 0 || len(h.Env) > 0 {
		cmd.Env = cmd.Environ() // the process's, with PWD set to Dir
		for _, env := range []map[string]string{r.Env, h.Env} {
			for _, k := range slices.Sorted(maps.Keys(env)) {
				cmd.Env = append(cmd.Env, k+"="+env[k])
			}
		}
	}
	cmd.Stdin = bytes.NewReader(payload)
	// Own process group so the timeout kills the whole tree, not just sh.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	stdout := &capBuffer{limit: maxHookOutput}
	stderr := &capBuffer{limit: maxHookOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	reapGroup(cmd)
	out, errOut := stdout.String(), stderr.String()
	combined := joinStreams(out, errOut)

	if runErr != nil {
		if ctx.Err() != nil {
			return Result{Decision: Proceed, ExitCode: -1, Output: combined, Err: fmt.Errorf("hookrun: hook timed out: %w", ctx.Err())}
		}
		if cmd.ProcessState == nil {
			return Result{Decision: Proceed, ExitCode: -1, Output: combined, Err: fmt.Errorf("hookrun: run hook: %w", runErr)}
		}
	}
	exit := cmd.ProcessState.ExitCode()
	switch exit {
	case 0:
		res := parseOutput(out)
		res.Output, res.ExitCode = combined, exit
		return res
	case blockingExit:
		reason := strings.TrimSpace(errOut)
		if parsed := parseOutput(out); parsed.Reason != "" {
			reason = parsed.Reason
		}
		return Result{Decision: Deny, Reason: reason, Output: combined, ExitCode: exit}
	}
	return Result{Decision: Proceed, Output: combined, ExitCode: exit,
		Err: fmt.Errorf("hookrun: hook exited %d: %s", exit, firstLine(errOut))}
}

// blockingExit is the exit code that blocks, as in Claude Code.
const blockingExit = 2

// firstLine is the first line of s, for an error message.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// reapGroup kills whatever is left of the hook's process group once Run
// returns. cmd.Cancel fires only on ctx cancellation; a hook that exits
// cleanly after backgrounding a child (`sleep 5 & exit 0`) leaves that
// child alive on the WaitDelay/ErrWaitDelay path. ESRCH = already gone.
func reapGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("hookrun: failed to reap lingering hook descendants", "err", err)
	}
}

// joinStreams renders captured stdout+stderr for display, dropping an
// empty side so the combined output has no stray blank line.
func joinStreams(out, errOut string) string {
	switch {
	case errOut == "":
		return out
	case out == "":
		return errOut
	default:
		return strings.TrimRight(out, "\n") + "\n" + errOut
	}
}

// buildCmd builds the exec command, honoring the authored form: a shell
// string runs via `sh -c`; an argv array, or a command with args, runs
// with no shell. Hooks run only for trusted plugins, so shell is acceptable.
func buildCmd(ctx context.Context, h hookspec.Hook) *exec.Cmd {
	c := h.Command
	switch {
	case len(h.Args) > 0:
		// Claude Code's exec form: command is the program, args its arguments.
		return exec.CommandContext(ctx, c.Parts[0], h.Args...)
	case c.Shell:
		return exec.CommandContext(ctx, "sh", "-c", strings.Join(c.Parts, " "))
	}
	return exec.CommandContext(ctx, c.Parts[0], c.Parts[1:]...)
}

// hookOutput is the JSON a hook may print on a clean exit.
type hookOutput struct {
	Continue           *bool  `json:"continue"`
	StopReason         string `json:"stopReason"`
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	HookSpecificOutput struct {
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
		AdditionalContext        string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// parseOutput reads a clean run's stdout: a JSON object with decisions,
// or plain text, which is context for the events that take it.
func parseOutput(out string) Result {
	trimmed := strings.TrimSpace(out)
	var ho hookOutput
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &ho) != nil {
		return Result{Decision: Proceed, Context: trimmed}
	}
	res := Result{Decision: Proceed, Reason: ho.Reason, Context: ho.HookSpecificOutput.AdditionalContext}
	if ho.Continue != nil && !*ho.Continue {
		res.Stop, res.StopReason = true, ho.StopReason
	}
	switch strings.ToLower(ho.Decision) {
	case "block", "deny":
		res.Decision = Deny
	case "approve", "allow":
		res.Decision = Allow
	}
	switch strings.ToLower(ho.HookSpecificOutput.PermissionDecision) {
	case "deny":
		res.Decision = Deny
	case "ask":
		res.Decision = Ask
	case "allow":
		res.Decision = Allow
	}
	if r := ho.HookSpecificOutput.PermissionDecisionReason; r != "" {
		res.Reason = r
	}
	return res
}

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
