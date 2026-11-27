package hookrun

import (
	"context"
	"os"
	"path/filepath"
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
		Input{HookEventName: "PreToolUse", ToolName: "Write"})
	if res.Decision != Deny || res.Reason != "not allowed" {
		t.Errorf("deny decision = %+v", res)
	}

	res = Runner{}.Run(context.Background(), cmdHook(`echo '{"decision":"approve"}'`), Input{})
	if res.Decision != Allow {
		t.Errorf("the legacy approve is an allow, got %+v", res)
	}
	res = Runner{}.Run(context.Background(),
		cmdHook(`echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"check it","additionalContext":"ctx"}}'`), Input{})
	if res.Decision != Ask || res.Reason != "check it" || res.Context != "ctx" {
		t.Errorf("hookSpecificOutput = %+v", res)
	}
	res = Runner{}.Run(context.Background(), cmdHook(`echo '{"continue":false,"stopReason":"done for today"}'`), Input{})
	if !res.Stop || res.StopReason != "done for today" {
		t.Errorf("continue false = %+v", res)
	}
}

// TestRun_ExitCodes: as in Claude Code, exit 2 blocks with stderr as the
// reason, another non-zero exit is an error that does not block, and a
// clean exit's plain stdout is context.
func TestRun_ExitCodes(t *testing.T) {
	t.Parallel()
	res := Runner{}.Run(context.Background(), cmdHook(`echo blocked >&2; exit 2`), Input{})
	if res.Decision != Deny || res.Reason != "blocked" || res.ExitCode != 2 {
		t.Errorf("exit 2 = %+v", res)
	}
	res = Runner{}.Run(context.Background(), cmdHook(`echo oops >&2; exit 1`), Input{})
	if res.Decision != Proceed || res.Err == nil || res.ExitCode != 1 {
		t.Errorf("exit 1 must not block, but report an error: %+v", res)
	}
	res = Runner{}.Run(context.Background(), cmdHook(`echo fine`), Input{})
	if res.Decision != Proceed || res.Context != "fine" {
		t.Errorf("zero exit = %+v, want proceed with the stdout as context", res)
	}
}

// TestRun_ExecFormAndDir: args run the command without a shell, in the
// runner's directory, with its environment.
func TestRun_ExecFormAndDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := hookspec.Hook{Type: hookspec.Command, Command: hookspec.CommandSpec{Parts: []string{"sh"}, Shell: true},
		Args: []string{"-c", `printf '%s %s' "$(pwd -P)" "$CLAUDE_PROJECT_DIR"`}}
	res := Runner{Dir: dir, Env: map[string]string{"CLAUDE_PROJECT_DIR": "/proj"}}.Run(context.Background(), h, Input{})
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Context != real+" /proj" {
		t.Errorf("output = %q, want the directory and the environment", res.Context)
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
	res := Runner{}.Run(context.Background(), cmdHook(`cat`), Input{HookEventName: "PreToolUse", ToolName: "Edit"})
	if !strings.Contains(res.Output, `"hook_event_name":"PreToolUse"`) || !strings.Contains(res.Output, `"tool_name":"Edit"`) {
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

// TestRun_TimeoutKillsBackgroundChild: a hook that backgrounds a child
// holding stdout must still return at the timeout (process-group kill +
// WaitDelay), not block until the child exits.
func TestRun_TimeoutKillsBackgroundChild(t *testing.T) {
	t.Parallel()
	start := time.Now()
	res := Runner{Timeout: 100 * time.Millisecond}.Run(context.Background(), cmdHook(`sleep 5 & sleep 5`), Input{})
	if res.Decision != Proceed || res.Err == nil {
		t.Errorf("timed-out hook should fail open with an error, got %+v", res)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run took %v; background child defeated the timeout", d)
	}
}

// TestRun_CleanExitReapsBackgroundChild: a hook that exits 0 after
// backgrounding a child holding stdout returns Proceed (ErrWaitDelay is
// not a failure) AND the child must not outlive Run. The descendant
// would touch a sentinel after 2s; if reaping worked it never does.
// (A pid probe can't tell a zombie from a live process, so the sentinel
// is the observable.) The dir travels via Env, not the shell string.
func TestRun_CleanExitReapsBackgroundChild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "lingered.txt")
	h := cmdHook(`( sleep 2; touch "$NIB_HOOK_TEST_DIR/lingered.txt" ) & printf done`)
	h.Env = map[string]string{"NIB_HOOK_TEST_DIR": dir}
	res := Runner{}.Run(context.Background(), h, Input{})
	if res.Decision != Proceed || res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("clean exit with lingering child should Proceed, got %+v", res)
	}
	// Long enough for the would-be sleep to elapse + filesystem flush.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("sentinel %q exists: background child survived Run", sentinel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
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
