package proc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunSuccess verifies a normal command completes with exit 0 and
// output captured.
func TestRunSuccess(t *testing.T) {
	t.Parallel()

	res := Run(context.Background(), Request{Command: "printf hello"})
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

	res := Run(context.Background(), Request{Command: "exit 7"})
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
		Command: "sleep 10",
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

	res := Run(ctx, Request{Command: "sleep 10"})
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

	res := Run(context.Background(), Request{Command: ""})
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

// TestRunDirRespected verifies the working directory is honoured.
func TestRunDirRespected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	res := Run(context.Background(), Request{
		Command: "pwd",
		Dir:     dir,
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
