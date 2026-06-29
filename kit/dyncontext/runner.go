package dyncontext

import (
	"context"
	"os/exec"
	"time"
)

// defaultRunTimeout bounds a single dynamic-context command so a hung
// directive cannot stall skill invocation indefinitely.
const defaultRunTimeout = 30 * time.Second

// ShellRunner executes commands via `sh -c`, supporting real shell
// syntax (quotes, pipes, redirects). It is only ever reached after a
// command clears the [toolperm.Matcher] gate, and only for plugins the
// user has explicitly trusted.
//
// CAVEAT: matcher globs match the whole command string, so a broad grant
// like `Bash(git *)` also matches a chained `git x; rm -rf y` because the
// `*` spans the separator. Tighter per-command parsing (argv-level gating
// that blocks shell chaining) is future hardening; today the trust gate
// (the user vouched for this plugin) is the backstop. Construct via
// [NewShellRunner].
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
// returning its combined stdout+stderr.
func (r ShellRunner) Run(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	return string(out), err
}
