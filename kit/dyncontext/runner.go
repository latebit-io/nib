package dyncontext

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// defaultRunTimeout bounds a single dynamic-context command so a hung
// directive cannot stall skill invocation indefinitely.
const defaultRunTimeout = 30 * time.Second

// maxRunOutput caps how much command output is inlined into the prompt.
// Dynamic-context output is fed straight into the LLM context, so it must
// have an explicit upper bound — a noisy command would otherwise exhaust
// memory or blow up the prompt long before the timeout helps.
const maxRunOutput = 64 << 10 // 64 KiB

// ShellRunner executes commands via `sh -c`, supporting real shell
// syntax (quotes, pipes, redirects). It is only ever reached after a
// command clears the [toolperm.Matcher] gate. It is the default runner
// for skill tools; an agent with a shell gate replaces it with a runner
// over its own bash tool (kit.ShellBinder), so in nib-code directives
// pass per-command approval as well.
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
func (r ShellRunner) Run(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	// Sharing one capWriter for both streams makes os/exec serialize the
	// writes (documented when Stdout == Stderr and the type is
	// comparable), so the cap is applied without a data race.
	w := &capWriter{limit: maxRunOutput}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	out := w.b.String()
	if w.truncated {
		out += fmt.Sprintf("\n[output truncated at %d bytes]", maxRunOutput)
	}
	return out, err
}

// capWriter accumulates up to limit bytes and discards the rest, so a
// runaway command cannot exhaust memory or the prompt budget.
type capWriter struct {
	b         strings.Builder
	limit     int
	n         int
	truncated bool
}

// Write records up to the cap and reports len(p) so the process is not
// blocked on a short write once the cap is reached.
func (w *capWriter) Write(p []byte) (int, error) {
	if rem := w.limit - w.n; rem > 0 {
		if len(p) <= rem {
			w.b.Write(p)
			w.n += len(p)
		} else {
			w.b.Write(p[:rem])
			w.n = w.limit
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}
