package dyncontext

import (
	"context"
	"fmt"
	"time"

	"github.com/latebit-io/nib/kit/proc"
)

// defaultRunTimeout bounds a single dynamic-context command so a hung
// directive cannot stall skill invocation indefinitely.
const defaultRunTimeout = 30 * time.Second

// maxRunOutput caps how much command output is inlined into the prompt.
// Dynamic-context output is fed straight into the LLM context, so it must
// have an explicit upper bound — a noisy command would otherwise exhaust
// memory or blow up the prompt long before the timeout helps. The cap is
// split into a head and tail so a long command still shows its ending
// (errors, summaries) rather than only its start.
const (
	maxRunOutput = 64 << 10 // 64 KiB
	maxRunTail   = 8 << 10  // last 8 KiB kept when output exceeds the cap
)

// ShellRunner executes commands via `sh -c`, supporting real shell
// syntax (quotes, pipes, redirects). It is only ever reached after a
// command clears the [toolperm.Matcher] gate. It is the default runner
// for skill tools; an agent with a shell gate replaces it with a runner
// over its own bash tool (kit.ShellBinder), so in nib-code directives
// pass per-command approval as well.
//
// Execution goes through [proc.Run], which runs the command in its own
// process group with a SIGKILL cancel and pipe-drain grace so a child
// holding stdout cannot defeat the timeout.
//
// CAVEAT: matcher globs match the whole command string, so a broad grant
// like `Bash(git *)` also matches a chained `git x; rm -rf y` because the
// `*` spans the separator. Tighter per-command parsing (argv-level gating
// that blocks shell chaining) is future hardening; today the agent's
// bash gate (approval, or the plugin trust vouch where a consumer wires
// no gate) is the backstop. Construct via [NewShellRunner].
type ShellRunner struct {
	timeout time.Duration
}

// NewShellRunner returns a ShellRunner with the given per-command
// timeout; a non-positive timeout uses [defaultRunTimeout].
func NewShellRunner(timeout time.Duration) ShellRunner {
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	return ShellRunner{timeout: timeout}
}

// Run implements [Runner], executing command under a timeout and
// returning its combined stdout+stderr, capped at [maxRunOutput].
// A zero-value ShellRunner (not built via [NewShellRunner]) still gets
// [defaultRunTimeout] rather than an instantly-firing deadline.
func (r ShellRunner) Run(ctx context.Context, command string) (string, error) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	res := proc.Run(ctx, proc.Request{
		Shell:   command,
		Timeout: timeout,
		HeadCap: maxRunOutput - maxRunTail,
		TailCap: maxRunTail,
	})
	switch {
	case res.StartErr != nil:
		return res.Output, res.StartErr
	case res.Cancelled:
		return res.Output, ctx.Err()
	case res.TimedOut:
		return res.Output, fmt.Errorf("timed out after %s", timeout)
	case res.ExitCode != 0:
		return res.Output, fmt.Errorf("exit status %d", res.ExitCode)
	}
	return res.Output, nil
}
