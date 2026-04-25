package proc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunSuccess verifies a normal command completes with exit 0 and
// output captured.
func TestRunSuccess(t *testing.T) {
	t.Parallel()

	res := Run(context.Background(), Request{Shell: "printf hello"})
	if res.StartErr != nil {
		t.Fatalf("StartErr = %v, want nil", res.StartErr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Output != "hello" {
		t.Errorf("Output = %q, want %q", res.Output, "hello")
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false")
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}
}

// TestRunNonzeroExit verifies a non-zero exit code is preserved.
func TestRunNonzeroExit(t *testing.T) {
	t.Parallel()

	res := Run(context.Background(), Request{Shell: "exit 7"})
	if res.StartErr != nil {
		t.Fatalf("StartErr = %v, want nil", res.StartErr)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

// TestRunTimeout verifies the timeout fires and the process group is
// killed. The sentinel uses `sleep 10` which would otherwise hang the
// test.
func TestRunTimeout(t *testing.T) {
	t.Parallel()

	res := Run(context.Background(), Request{
		Shell:   "sleep 10",
		Timeout: 100 * time.Millisecond,
	})
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true; res=%+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 on timeout", res.ExitCode)
	}
	if res.Duration > time.Second {
		t.Errorf("Duration = %v, want < 1s — process group not killed?", res.Duration)
	}
}

// TestRunCancelled verifies parent-context cancellation reports
// Cancelled (not TimedOut), letting callers distinguish "user pressed
// Esc" from "command took too long."
func TestRunCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	res := Run(ctx, Request{Shell: "sleep 10"})
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false on cancel")
	}
	if !res.Cancelled {
		t.Errorf("Cancelled = false, want true")
	}
}

// TestRunEmptyCommand verifies the sentinel error path.
func TestRunEmptyCommand(t *testing.T) {
	t.Parallel()

	res := Run(context.Background(), Request{Shell: ""})
	if res.StartErr != ErrEmptyCommand {
		t.Errorf("StartErr = %v, want ErrEmptyCommand", res.StartErr)
	}
}

// TestHeadTailNoTruncation verifies output below HeadCap is returned
// verbatim.
func TestHeadTailNoTruncation(t *testing.T) {
	t.Parallel()

	w := NewHeadTailWriter(100, 100)
	_, _ = w.Write([]byte("hello world"))
	if got := w.String(); got != "hello world" {
		t.Errorf("String() = %q, want %q", got, "hello world")
	}
}

// TestHeadTailTruncation verifies the collapse marker appears when
// total output exceeds HeadCap + TailCap. The first HeadCap bytes and
// the last TailCap bytes are preserved.
func TestHeadTailTruncation(t *testing.T) {
	t.Parallel()

	w := NewHeadTailWriter(10, 10)
	// 30 bytes of distinct content so we can verify head and tail.
	head := "AAAAAAAAAA" // 10 A's
	mid := "BBBBBBBBBB"  // 10 B's (will be discarded)
	tail := "CCCCCCCCCC" // 10 C's
	all := []byte(head + mid + tail)
	_, _ = w.Write(all)

	got := w.String()
	if !strings.HasPrefix(got, head) {
		t.Errorf("output does not start with head; got %q", got)
	}
	if !strings.HasSuffix(got, tail) {
		t.Errorf("output does not end with tail; got %q", got)
	}
	if !strings.Contains(got, "10 bytes collapsed") {
		t.Errorf("output missing collapse marker; got %q", got)
	}
}

// TestRunParentDeadlineSurfacesAsCancelled verifies that when the
// parent context expires (deadline shorter than Request.Timeout),
// the result reports Cancelled rather than TimedOut. Without the
// child-first-check fix, the inherited DeadlineExceeded on cmdCtx
// would be mis-attributed to Request.Timeout firing.
func TestRunParentDeadlineSurfacesAsCancelled(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	res := Run(parent, Request{
		Shell:   "sleep 10",
		Timeout: 30 * time.Second, // far longer than the parent deadline
	})

	if !res.Cancelled {
		t.Errorf("Cancelled = false, want true (parent deadline must surface as Cancelled, not TimedOut)")
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false (Request.Timeout did not fire)")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestRunWaitDelayBackgroundedChildSucceeds verifies the
// exec.ErrWaitDelay path is treated as success rather than a
// StartErr. The shell command exits cleanly while a backgrounded
// `sleep` still holds the stdout pipe — exec.Run surfaces this as
// ErrWaitDelay after WaitDelay (1s by default in proc) expires, but
// the parent process succeeded so ExitCode must be 0.
//
// Without the explicit ErrWaitDelay branch in Run, this test would
// fail because the error would land in StartErr ("proc: …
// ErrWaitDelay"), mislabeling a clean exit as a fork failure.
func TestRunWaitDelayBackgroundedChildSucceeds(t *testing.T) {
	t.Parallel()

	// `( sleep 5 & ) 2>/dev/null` backgrounds sleep with no output;
	// `printf done` forces final stdout that the parent uses cleanly.
	// The parent exits 0 immediately; the daemonised sleep keeps a
	// pipe open until WaitDelay (1s) forces close → ErrWaitDelay.
	res := Run(context.Background(), Request{
		Shell:   "( sleep 5 ) & printf done",
		Timeout: 10 * time.Second,
	})
	if res.StartErr != nil {
		t.Fatalf("StartErr = %v, want nil (ErrWaitDelay must not surface as StartErr)", res.StartErr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (process exited successfully despite pipe linger)", res.ExitCode)
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true, want false (timeout was 10s, command exits in <1s)")
	}
}

// TestRunWaitDelayReapsDescendants verifies that a backgrounded
// pipe-holding child is killed via the process group on the
// ErrWaitDelay path. Without explicit reaping, the descendant
// survives Run's return and pollutes subsequent invocations
// (stealing ports, mutating shared state). The test uses a
// sentinel-touch trick: the backgrounded process tries to write a
// file 2 seconds after fork. If the file exists when we look, the
// process survived and the regression has returned.
func TestRunWaitDelayReapsDescendants(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "lingered.txt")

	// `( sleep 2; touch SENTINEL )` is the descendant. `printf done`
	// makes the parent shell exit cleanly. WaitDelay (1s) fires
	// because the descendant still holds the pipe → ErrWaitDelay
	// path → must reap.
	res := Run(context.Background(), Request{
		Shell:   fmt.Sprintf("( sleep 2; touch %s ) & printf done", sentinel),
		Timeout: 10 * time.Second,
	})
	if res.StartErr != nil {
		t.Fatalf("StartErr = %v", res.StartErr)
	}

	// Wait long enough for the would-be sleep to elapse + filesystem
	// flush. If reaping worked, the touch never fires.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(sentinel); err == nil {
		t.Errorf("sentinel %q exists — backgrounded descendant survived ErrWaitDelay reaping", sentinel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}
}

// TestNewHeadTailWriterDefaultsZeroAndNegative verifies the constructor
// substitutes DefaultHead / DefaultTail when given zero or negative
// caps. Without this, tailCap=0 would make Write spin forever (chunk
// size stays zero in the ring-buffer copy) and negative values would
// panic in make([]byte, …).
func TestNewHeadTailWriterDefaultsZeroAndNegative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		headCap, tailCap int
	}{
		{"both zero", 0, 0},
		{"both negative", -100, -50},
		{"head zero only", 0, 100},
		{"tail negative only", 100, -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewHeadTailWriter(tc.headCap, tc.tailCap)
			// Write some bytes — must not panic, must not spin forever,
			// must produce a non-empty output via String().
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = w.Write([]byte("hello world"))
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatalf("Write spun forever or blocked — caps did not default to positive values")
			}
			if w.String() == "" {
				t.Errorf("String() empty after write; defaulted caps should hold output")
			}
		})
	}
}

// TestRunDefaultsHeadTailViaWriter verifies that a Request with zero
// HeadCap/TailCap still produces useful output — the defaulting now
// lives inside NewHeadTailWriter, not Run, so this confirms the
// single-source-of-truth refactor preserves behaviour.
func TestRunDefaultsHeadTailViaWriter(t *testing.T) {
	t.Parallel()

	res := Run(context.Background(), Request{
		Shell: "printf hello",
		// HeadCap and TailCap left zero — should default to DefaultHead/DefaultTail.
	})
	if res.StartErr != nil {
		t.Fatalf("StartErr = %v", res.StartErr)
	}
	if res.Output != "hello" {
		t.Errorf("Output = %q, want %q", res.Output, "hello")
	}
}

// TestRunDirRespected verifies the working directory is honoured.
func TestRunDirRespected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	res := Run(context.Background(), Request{
		Shell: "pwd",
		Dir:   dir,
	})
	if res.StartErr != nil {
		t.Fatalf("StartErr = %v", res.StartErr)
	}
	// On macOS /tmp is symlinked to /private/tmp; pwd may report the
	// resolved path. Substring match keeps the test portable.
	if !strings.HasSuffix(strings.TrimSpace(res.Output), strings.TrimPrefix(dir, "/private")) &&
		!strings.HasSuffix(strings.TrimSpace(res.Output), dir) {
		t.Errorf("pwd = %q, expected suffix %q", res.Output, dir)
	}
}
