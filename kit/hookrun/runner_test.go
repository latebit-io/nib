package hookrun

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/hookspec"
)

func cmdHook(command string) hookspec.Hook {
	return hookspec.Hook{Type: hookspec.Command, Command: hookspec.CommandSpec{Parts: []string{command}, Shell: true}}
}

func TestRun_JSONDecision(t *testing.T) {
	t.Parallel()
	res := Runner{}.Run(context.Background(),
		cmdHook(`echo '{"decision":"deny","reason":"not allowed"}'`),
		Input{Event: "PreToolUse", Tool: "write_file"})
	if res.Decision != Deny || res.Reason != "not allowed" {
		t.Errorf("deny decision = %+v", res)
	}

	res = Runner{}.Run(context.Background(), cmdHook(`echo '{"decision":"allow"}'`), Input{})
	if res.Decision != Proceed {
		t.Errorf("allow → proceed, got %+v", res)
	}
}

func TestRun_ExitCodeFallback(t *testing.T) {
	t.Parallel()
	// Non-zero exit with no JSON → Deny, output as reason.
	res := Runner{}.Run(context.Background(), cmdHook(`echo blocked >&2; exit 2`), Input{})
	if res.Decision != Deny || !strings.Contains(res.Reason, "blocked") || res.ExitCode != 2 {
		t.Errorf("non-zero exit = %+v", res)
	}

	// Zero exit, non-JSON output → Proceed.
	res = Runner{}.Run(context.Background(), cmdHook(`echo fine`), Input{})
	if res.Decision != Proceed {
		t.Errorf("zero exit → proceed, got %+v", res)
	}
}

func TestRun_DecisionFromStdoutNotStderr(t *testing.T) {
	t.Parallel()
	// Deny JSON on stdout, diagnostics on stderr, exit 0. The decision must
	// come from stdout (Deny), not fall back to the exit code (Proceed).
	res := Runner{}.Run(context.Background(),
		cmdHook(`echo '{"decision":"deny","reason":"blocked"}'; echo "some warning" >&2`),
		Input{})
	if res.Decision != Deny || res.Reason != "blocked" {
		t.Errorf("decision must parse from stdout only, got %+v", res)
	}
	if !strings.Contains(res.Output, "some warning") {
		t.Errorf("stderr should still appear in Output: %q", res.Output)
	}
}

func TestRun_StdinDelivered(t *testing.T) {
	t.Parallel()
	// `cat` echoes the JSON payload; assert the event reached stdin.
	res := Runner{}.Run(context.Background(), cmdHook(`cat`), Input{Event: "PreToolUse", Tool: "edit_file"})
	if !strings.Contains(res.Output, "PreToolUse") || !strings.Contains(res.Output, "edit_file") {
		t.Errorf("input not delivered on stdin: %q", res.Output)
	}
}

func TestRun_Env(t *testing.T) {
	t.Parallel()
	h := cmdHook(`printf '%s' "$HOOK_X"`)
	h.Env = map[string]string{"HOOK_X": "from-env"}
	res := Runner{}.Run(context.Background(), h, Input{})
	if !strings.Contains(res.Output, "from-env") {
		t.Errorf("hook env not applied: %q", res.Output)
	}
}

func TestRun_NonRunnableIsNoOp(t *testing.T) {
	t.Parallel()
	res := Runner{}.Run(context.Background(), hookspec.Hook{Type: hookspec.HTTP, URL: "https://x"}, Input{})
	if res.Decision != Proceed || res.ExitCode != -1 {
		t.Errorf("non-runnable hook should be a no-op proceed, got %+v", res)
	}
}

func TestRun_TimeoutFailsOpen(t *testing.T) {
	t.Parallel()
	res := Runner{Timeout: 50 * time.Millisecond}.Run(context.Background(), cmdHook(`sleep 5`), Input{})
	if res.Decision != Proceed || res.Err == nil {
		t.Errorf("timed-out hook should fail open with an error, got %+v", res)
	}
}

func TestCapBuffer(t *testing.T) {
	t.Parallel()
	w := &capBuffer{limit: 8}
	_, _ = w.Write([]byte("0123456789"))
	if !w.truncated || !strings.HasPrefix(w.String(), "01234567") || !strings.Contains(w.String(), "truncated") {
		t.Errorf("capBuffer = %q truncated=%t", w.String(), w.truncated)
	}
}
