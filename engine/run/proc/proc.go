// Package proc runs external processes with the defensive defaults the
// agent loop needs: a process group so child processes are reaped on
// timeout, a SIGKILL cancel hook for context cancellation, a grace
// period for pipe drainage, and a [HeadTailWriter] that bounds memory
// for high-volume output.
//
// The package is the shared substrate for [agent.BashTool] and the
// smoke-run validator. Both invoke external commands with timeouts and
// must not block forever on a child that holds a stdout pipe; the
// patterns here have been hardened against `go run .` and similar
// fork-and-exit shells.
//
// # Shell-only by design
//
// [Run] always invokes the command via `sh -c <Request.Shell>` — there
// is no argv mode. Both current callers (BashTool, smoke runner) are
// shell-by-design: BashTool exists so the LLM can use pipes,
// redirection, and command substitution; smoke commands frequently
// chain build-and-launch with `&&`. Sanitisation belongs at the
// caller's trust boundary, not in this helper — see
// [agent.BashTool] (LLM-authored, prompt-level guard only) and
// [agent.SmokeRunTool] (`.project/run.json`, reviewed and committed).
// If a future caller ever needs argv-style execution, add a separate
// API; do not add a mode flag that would let untrusted input slip
// through this entry point with shell semantics still implied.
package proc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultHead is the byte budget for the start of captured output.
const DefaultHead = 4 * 1024

// DefaultTail is the byte budget for the end of captured output.
const DefaultTail = 4 * 1024

// DefaultGraceAfterCancel is how long to wait for pipes to drain after
// a context cancellation kills the process group. Without this,
// [exec.Cmd.Run] may hang if a child still holds an inherited pipe fd.
const DefaultGraceAfterCancel = time.Second

// Request describes one process invocation. The command is always
// interpreted by `sh -c` — see the package doc for the rationale and
// the trust boundary expected of callers.
type Request struct {
	// Shell is the shell command line passed to `sh -c`. Callers may
	// use pipes, redirection, command substitution, env assignments,
	// and chained commands. The field is named Shell (not Command)
	// to make the interpretation impossible to confuse with
	// argv-style execution — there is no argv mode in this package.
	// Empty Shell yields [ErrEmptyCommand] before any process is started.
	Shell string

	// Dir is the working directory for the process. Empty means the
	// caller's current directory; pass an absolute path to be safe.
	Dir string

	// Timeout caps total wall-clock duration. A zero or negative
	// value falls back to [DefaultTimeout].
	Timeout time.Duration

	// HeadCap and TailCap override the head/tail capture sizes. Zero
	// values use [DefaultHead] and [DefaultTail].
	HeadCap int
	TailCap int

	// Env, when non-nil, replaces the inherited environment for the
	// child process. Nil inherits the parent's environment.
	Env []string
}

// DefaultTimeout is the wall-clock cap when [Request.Timeout] is zero.
const DefaultTimeout = 30 * time.Second

// Result is the outcome of a [Run] invocation.
type Result struct {
	// Output is the combined stdout + stderr captured via a
	// [HeadTailWriter]. The middle is collapsed into a marker line
	// when total output exceeds HeadCap + TailCap.
	Output string

	// ExitCode is the process exit code; -1 means the process was
	// signalled (timeout or context cancellation) before exiting
	// normally.
	ExitCode int

	// TimedOut is true when [Request.Timeout] specifically fired
	// before the process exited cleanly. Parent-context deadlines
	// (a caller passing `context.WithDeadline(...)` shorter than
	// Timeout) surface as Cancelled instead — TimedOut is reserved
	// for "the wall-clock cap *this package* applied was reached."
	// The process group has been signalled in that case; Output
	// may contain partial data.
	TimedOut bool

	// Cancelled is true when the parent context's Done channel
	// closed for any reason other than [Request.Timeout]: an
	// explicit ctx cancel (the developer hit Esc, the agent run
	// was aborted) OR a parent-supplied deadline that expired
	// before this package's own timeout. Both surface as
	// Cancelled because callers (BashTool, smoke-run) treat them
	// identically — the work was abandoned by the controlling
	// scope, not killed by proc itself.
	Cancelled bool

	// Duration is the wall-clock time the process ran for.
	Duration time.Duration

	// StartErr is set when the process could not be started at all
	// (exec lookup failure, fork error). Disjoint from ExitCode.
	StartErr error
}

// ErrEmptyCommand is returned by [Run] when [Request.Shell] is empty.
// Distinct from a process that exits without producing output so
// callers can fail loud rather than silent.
var ErrEmptyCommand = errors.New("proc: empty command")

// Run executes req under ctx and captures bounded output. Always
// returns a Result; check [Result.StartErr] for fork/exec failures
// and [Result.TimedOut] for deadline-driven kills.
//
// Concurrency: safe to call from multiple goroutines simultaneously.
// Each call gets its own subprocess and capture buffer.
func Run(ctx context.Context, req Request) Result {
	if req.Shell == "" {
		return Result{StartErr: ErrEmptyCommand}
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", req.Shell)
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = req.Env
	}

	// Run in its own process group so cmd.Cancel kills not just the
	// shell but every descendant. `go run .` and `make run` are the
	// canonical reasons this matters — they fork helpers that hold
	// the stdout pipe and would otherwise prevent Run() from returning.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = DefaultGraceAfterCancel

	htw := NewHeadTailWriter(req.HeadCap, req.TailCap)
	cmd.Stdout = htw
	cmd.Stderr = htw

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	out := htw.String()

	res := Result{Output: out, Duration: elapsed}

	switch {
	// Parent ctx checks come first. A caller-supplied cancellation
	// or deadline propagates to cmdCtx as DeadlineExceeded too, so
	// inspecting the child first would mis-attribute a parent-side
	// expiry to Request.Timeout. Both flavours of parent expiry
	// surface as Cancelled — the work was abandoned by the caller's
	// scope, not killed by this package.
	case ctx.Err() != nil:
		res.Cancelled = true
		res.ExitCode = -1
	case errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = -1
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The process itself exited successfully but inherited
		// pipes were still open when WaitDelay expired (typically:
		// a backgrounded child holds the stdout pipe). exec.Run
		// surfaces this as ErrWaitDelay; from the caller's
		// perspective the command succeeded.
		//
		// `cmd.Cancel` (which kills the whole process group via
		// SIGKILL on -PID) is invoked only on ctx cancellation,
		// NOT on the WaitDelay path. So an inherited-pipe-holder
		// (backgrounded daemon, double-forked child) survives the
		// call by default — and would steal ports, mutate shared
		// state, or otherwise pollute subsequent smoke runs.
		// Reap explicitly here. ESRCH means the group is already
		// gone (race with natural exit), which is fine.
		if cmd.Process != nil {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				slog.Warn("proc: failed to reap lingering descendants after WaitDelay",
					"err", err, "duration", elapsed)
			}
		}
		// Log without `req.Shell` — slog reaches /tmp/junto-debug.log
		// under --debug, and that file gets attached to bug reports,
		// pasted into chat threads, etc. Per CLAUDE.md no API keys or
		// secrets in debug output. Duration is the diagnostic signal:
		// short = drain-tax noise, long = something genuinely linger-y.
		slog.Warn("proc: WaitDelay expired with pipes still open after clean exit",
			"duration", elapsed)
		res.ExitCode = 0
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.StartErr = fmt.Errorf("proc: %w", runErr)
		}
	}

	return res
}

// HeadTailWriter captures the first HeadCap bytes and the last TailCap
// bytes of a stream, discarding the middle. Preserves both initial
// context and tail (errors, stack traces) while bounding memory.
//
// When total output fits within HeadCap, no truncation occurs and the
// tail buffer is unused. Once head fills, subsequent writes go into a
// circular ring buffer that always retains the most recent TailCap
// bytes.
type HeadTailWriter struct {
	mu       sync.Mutex
	head     []byte
	headCap  int
	tail     []byte
	tailCap  int
	tailPos  int
	tailFull bool
	total    int
}

// NewHeadTailWriter creates a writer that keeps the first headCap bytes
// and the last tailCap bytes of output. A zero or negative argument
// is replaced by [DefaultHead] or [DefaultTail] respectively — the
// zero value of the constructor is useful, matching the rest of the
// package's API style. The defaulting is enforced HERE rather than
// in callers because tailCap == 0 would otherwise cause Write to
// spin forever (the chunk size in the ring-buffer copy would stay
// zero, never advancing through the input).
func NewHeadTailWriter(headCap, tailCap int) *HeadTailWriter {
	if headCap <= 0 {
		headCap = DefaultHead
	}
	if tailCap <= 0 {
		tailCap = DefaultTail
	}
	return &HeadTailWriter{
		head:    make([]byte, 0, headCap),
		headCap: headCap,
		tail:    make([]byte, tailCap),
		tailCap: tailCap,
	}
}

// Write implements [io.Writer]. Always returns len(p), nil so the
// subprocess never stalls on a blocked pipe. Safe for concurrent use —
// stdout and stderr are drained by separate goroutines in [os/exec].
func (w *HeadTailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(p)
	w.total += n

	if len(w.head) < w.headCap {
		room := w.headCap - len(w.head)
		if room >= len(p) {
			w.head = append(w.head, p...)
			return n, nil
		}
		w.head = append(w.head, p[:room]...)
		p = p[room:]
	}

	for len(p) > 0 {
		chunk := min(w.tailCap-w.tailPos, len(p))
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

// String returns the captured output. If no truncation occurred,
// returns the head buffer only. Otherwise returns head + a collapse
// marker + tail. Must be called after the process has exited (no
// concurrent writes) so the lock release does not race the writer
// goroutines.
func (w *HeadTailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	headStr := string(w.head)
	if w.total <= w.headCap {
		return headStr
	}

	var tailStr string
	if w.tailFull {
		tailStr = string(w.tail[w.tailPos:]) + string(w.tail[:w.tailPos])
	} else {
		tailStr = string(w.tail[:w.tailPos])
	}

	dropped := w.total - len(w.head) - len(tailStr)
	if dropped <= 0 {
		return headStr + tailStr
	}
	return fmt.Sprintf(
		"%s\n\n[... %d bytes collapsed — showing first %d and last %d bytes ...]\n\n%s",
		headStr, dropped, len(w.head), len(tailStr), tailStr,
	)
}
