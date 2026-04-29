package approval

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shortTimeout is the cap on Await* calls in tests that should return
// quickly. Long enough to absorb scheduling jitter on a busy CI runner,
// short enough that a regression where the await blocks indefinitely
// fails the test promptly.
const shortTimeout = 200 * time.Millisecond

func TestCoordinator_AwaitApproval_Approved(t *testing.T) {
	t.Parallel()
	c := New()
	c.Approve()
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	approved, err := c.AwaitApproval(ctx)
	if err != nil {
		t.Fatalf("AwaitApproval err = %v, want nil", err)
	}
	if !approved {
		t.Errorf("AwaitApproval approved = false, want true")
	}
}

func TestCoordinator_AwaitApproval_Rejected(t *testing.T) {
	t.Parallel()
	c := New()
	c.Reject()
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	approved, err := c.AwaitApproval(ctx)
	if err != nil {
		t.Fatalf("AwaitApproval err = %v, want nil", err)
	}
	if approved {
		t.Errorf("AwaitApproval approved = true, want false")
	}
}

func TestCoordinator_AwaitApproval_CtxCancel(t *testing.T) {
	t.Parallel()
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := c.AwaitApproval(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AwaitApproval err = %v, want DeadlineExceeded", err)
	}
}

func TestCoordinator_AwaitContinue_DeliversContent(t *testing.T) {
	t.Parallel()
	c := New()
	want := "buffer state after approval"
	c.Continue(want)
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	got, err := c.AwaitContinue(ctx)
	if err != nil {
		t.Fatalf("AwaitContinue err = %v, want nil", err)
	}
	if got != want {
		t.Errorf("AwaitContinue content = %q, want %q", got, want)
	}
}

func TestCoordinator_AwaitInput_DeliversReply(t *testing.T) {
	t.Parallel()
	c := New()
	if !c.Reply("next question") {
		t.Fatal("Reply returned false on empty buffer, want true")
	}
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	got, err := c.AwaitInput(ctx)
	if err != nil {
		t.Fatalf("AwaitInput err = %v, want nil", err)
	}
	if got != "next question" {
		t.Errorf("AwaitInput got %q, want %q", got, "next question")
	}
}

func TestCoordinator_Reply_DropsWhenFull(t *testing.T) {
	t.Parallel()
	c := New()
	if !c.Reply("first") {
		t.Fatal("first Reply returned false, want true")
	}
	if c.Reply("second") {
		t.Errorf("second Reply returned true, want false (channel full)")
	}
	// Drain so the test cleans up after itself.
	c.Reset()
}

func TestCoordinator_AwaitAnswer_DeliversText(t *testing.T) {
	t.Parallel()
	c := New()
	c.Answer("option-a")
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	got, err := c.AwaitAnswer(ctx)
	if err != nil {
		t.Fatalf("AwaitAnswer err = %v, want nil", err)
	}
	if got != "option-a" {
		t.Errorf("AwaitAnswer got %q, want %q", got, "option-a")
	}
}

func TestCoordinator_Answer_DropsWhenFull(t *testing.T) {
	t.Parallel()
	c := New()
	c.Answer("first")
	c.Answer("second-dropped") // capacity 1, second is silently dropped
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	got, err := c.AwaitAnswer(ctx)
	if err != nil {
		t.Fatalf("AwaitAnswer err = %v, want nil", err)
	}
	if got != "first" {
		t.Errorf("AwaitAnswer got %q, want first (second.Answer should be dropped, not overwrite)", got)
	}
}

func TestCoordinator_Reset_DrainsAllChannels(t *testing.T) {
	t.Parallel()
	c := New()
	c.Approve()
	c.Continue("stale")
	c.Reply("stale-reply")
	c.Answer("stale-answer")

	c.Reset()

	// Each channel should now be empty — verify with a non-blocking poll.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.AwaitApproval(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("approveCh not drained: err=%v, want DeadlineExceeded", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if _, err := c.AwaitContinue(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("continueCh not drained: err=%v, want DeadlineExceeded", err)
	}

	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel3()
	if _, err := c.AwaitInput(ctx3); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("inputCh not drained: err=%v, want DeadlineExceeded", err)
	}

	ctx4, cancel4 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel4()
	if _, err := c.AwaitAnswer(ctx4); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("answerCh not drained: err=%v, want DeadlineExceeded", err)
	}
}

func TestCoordinator_SignalsAreNonBlocking(t *testing.T) {
	t.Parallel()
	c := New()
	// Saturate every channel; subsequent signals must NOT block.
	c.Approve()
	c.Continue("saturated")
	c.Reply("saturated")
	c.Answer("saturated")

	done := make(chan struct{})
	go func() {
		c.Approve() // already-saturated approveCh — must drop, not block
		c.Reject()  // approveCh still saturated by the earlier Approve
		c.Continue("dropped")
		c.Reply("dropped") // Reply returns false; not measured here
		c.Answer("dropped")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shortTimeout):
		t.Fatal("signal methods blocked when channels were saturated")
	}
}

func TestCoordinator_AwaitContinue_CtxCancel(t *testing.T) {
	t.Parallel()
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.AwaitContinue(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AwaitContinue err = %v, want DeadlineExceeded", err)
	}
}

func TestCoordinator_AwaitInput_CtxCancel(t *testing.T) {
	t.Parallel()
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.AwaitInput(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AwaitInput err = %v, want DeadlineExceeded", err)
	}
}

func TestCoordinator_AwaitAnswer_CtxCancel(t *testing.T) {
	t.Parallel()
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.AwaitAnswer(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AwaitAnswer err = %v, want DeadlineExceeded", err)
	}
}

// TestDrain_ClosedChannelTerminates locks the defensive guarantee
// that drain returns promptly on a closed channel. A closed receive
// is always ready in a select arm, so a naive
// `for { select { case <-ch: ; default: return } }` spins forever
// once the channel is closed. The two-value receive + ok-flag check
// terminates the loop. Coordinator never closes its own channels,
// but drain is a generic helper that should not deadlock the test
// harness if a caller ever does.
func TestDrain_ClosedChannelTerminates(t *testing.T) {
	t.Parallel()
	ch := make(chan int, 3)
	ch <- 1
	ch <- 2
	close(ch) // emptied first iteration of drain via the values, then ok=false

	done := make(chan struct{})
	go func() {
		drain(ch)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shortTimeout):
		t.Fatal("drain did not terminate on a closed channel within the timeout")
	}
}
