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
package proc

import (
	"context"
	"errors"
	"fmt"
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

// Request describes one process invocation.
type Request struct {
	// Command is passed to `sh -c` so callers can use shell features
	// (pipes, redirection, substitution). Empty Command yields an
	// error before any process is started.
	Command string

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

	// TimedOut is true when the timeout fired before the process
	// exited cleanly. The process group has been signalled in that
	// case; Output may contain partial data.
	TimedOut bool

	// Cancelled is true when the parent context was cancelled (not
	// the timeout). Distinguished from TimedOut so callers can
	// surface "user pressed Esc" differently from "command took too
	// long."
	Cancelled bool

	// Duration is the wall-clock time the process ran for.
	Duration time.Duration

	// StartErr is set when the process could not be started at all
	// (exec lookup failure, fork error). Disjoint from ExitCode.
	StartErr error
}

// ErrEmptyCommand is returned by [Run] when [Request.Command] is empty.
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
	if req.Command == "" {
		return Result{StartErr: ErrEmptyCommand}
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	headCap := req.HeadCap
	if headCap <= 0 {
		headCap = DefaultHead
	}
	tailCap := req.TailCap
	if tailCap <= 0 {
		tailCap = DefaultTail
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", req.Command)
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

	htw := NewHeadTailWriter(headCap, tailCap)
	cmd.Stdout = htw
	cmd.Stderr = htw

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	out := htw.String()

	res := Result{Output: out, Duration: elapsed}

	switch {
	case errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.ExitCode = -1
	case ctx.Err() != nil:
		res.Cancelled = true
		res.ExitCode = -1
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
// and the last tailCap bytes of output. Both arguments must be positive.
func NewHeadTailWriter(headCap, tailCap int) *HeadTailWriter {
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
