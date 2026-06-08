package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
)

// TestScrollToBottom_SnapWhenIdle verifies that auto-scroll-to-bottom
// snaps immediately when no streaming-driven tick loop is in flight.
// Without a tick, there is nothing to advance the ease — the only
// honest outcome is to land at the target on the same frame.
func TestScrollToBottom_SnapWhenIdle(t *testing.T) {
	m := beatPane()
	// Fill enough lines that there's a scroll position to reach.
	for range 50 {
		m.AppendText("line\n")
	}
	m.ScrollOffset = 0 // pretend we scrolled up; auto-scroll should bring us back

	m.scrollToBottom()

	if m.ScrollOffset != m.scrollTarget {
		t.Errorf("idle scrollToBottom did not snap: ScrollOffset=%d target=%d",
			m.ScrollOffset, m.scrollTarget)
	}
}

// TestScrollToBottom_DefersToTickWhileStreaming verifies that during
// streaming (a spinner tick loop in flight), scrollToBottom records
// the target but does NOT snap. The tick handler is responsible for
// advancing — snapping here would re-introduce the per-token jump
// step 9 was supposed to eliminate.
func TestScrollToBottom_DefersToTickWhileStreaming(t *testing.T) {
	m := beatPane()
	for range 50 {
		m.AppendText("line\n")
	}
	m.ScrollOffset = 0
	// Simulate the streaming state: animated status + spinner loop alive.
	m.SetStatus(event.StatusThinking)
	m.spinnerRunning = true

	m.scrollToBottom()

	if m.ScrollOffset == m.scrollTarget {
		t.Errorf("streaming scrollToBottom snapped — should defer to tick: ScrollOffset=%d target=%d",
			m.ScrollOffset, m.scrollTarget)
	}
	if m.scrollTarget == 0 {
		t.Errorf("scrollTarget = 0; want > 0 after 50 lines appended")
	}
}

// TestStreamingKeepsFollowingBottomMidEase is the regression test for the
// "scrollbar does not follow the output" bug: tokens arriving while
// ScrollOffset was still easing toward the bottom read the lagging offset
// (via isAtBottom) as "scrolled up", so auto-follow disengaged and
// scrollTarget froze above the true bottom — the view and scrollbar
// stalled while output kept arriving below.
func TestStreamingKeepsFollowingBottomMidEase(t *testing.T) {
	m := beatPane()
	for range 50 {
		m.AppendText("line\n")
	}
	// Enter streaming so scrollToBottom defers to the ease, then force the
	// mid-ease state: target at the bottom, ScrollOffset lagging at the top.
	m.SetStatus(event.StatusThinking)
	m.spinnerRunning = true
	m.scrollToBottom()
	m.ScrollOffset = 0

	if m.scrollTarget == 0 {
		t.Fatal("precondition: scrollTarget should sit at the bottom")
	}
	if m.isAtBottom() {
		t.Fatal("precondition: literal isAtBottom must be false mid-ease")
	}
	if !m.isFollowingBottom() {
		t.Fatal("isFollowingBottom must be true while auto-scroll is in flight")
	}

	// New output must keep the target tracking the growing bottom.
	prevTarget := m.scrollTarget
	for range 10 {
		m.AppendText("more\n")
	}
	if m.scrollTarget <= prevTarget {
		t.Errorf("scrollTarget did not advance with new output (%d → %d): auto-follow disengaged mid-ease",
			prevTarget, m.scrollTarget)
	}
}

// TestAdvanceScrollEase_StepsTowardTarget verifies the per-tick ease
// math: each frame moves ~40% of the remaining delta, with a snap
// threshold so we never stall sub-line short of the target. Stepping
// from 0 toward a far target should produce a strictly monotonic
// sequence that eventually reaches it.
func TestAdvanceScrollEase_StepsTowardTarget(t *testing.T) {
	m := beatPane()
	m.ScrollOffset = 0
	m.scrollTarget = 30

	prev := m.ScrollOffset
	for i := range 20 {
		m.advanceScrollEase()
		if m.ScrollOffset < prev {
			t.Fatalf("frame %d: ScrollOffset moved backwards: %d → %d", i, prev, m.ScrollOffset)
		}
		if m.ScrollOffset == m.scrollTarget {
			return // settled
		}
		prev = m.ScrollOffset
	}
	t.Errorf("did not settle in 20 frames: ScrollOffset=%d target=%d", m.ScrollOffset, m.scrollTarget)
}

// TestMouseWheel_DuringStreaming_DoesNotFightEase locks down the
// "stop fighting the wheel" contract. While streaming, the spinner
// tick eases ScrollOffset toward scrollTarget every 100ms. If the
// user wheel-scrolls up, both ScrollOffset *and* scrollTarget must
// move together — otherwise the next tick drags them back toward
// the previous auto-bottom target. Same contract applies to keyboard
// arrow scrolls.
func TestMouseWheel_DuringStreaming_DoesNotFightEase(t *testing.T) {
	m := beatPane()
	for range 50 {
		m.AppendText("line\n")
	}
	m.SetStatus(event.StatusThinking)
	m.spinnerRunning = true
	m.scrollToBottom() // sets scrollTarget to bottom; ScrollOffset stays mid-ease

	// Pretend the ease has caught up: both at bottom.
	m.ScrollOffset = m.scrollTarget
	bottom := m.scrollTarget

	// User wheel-scrolls up.
	m.handleMouseWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})

	if m.ScrollOffset >= bottom {
		t.Fatalf("wheel-up did not move ScrollOffset away from bottom (was %d, still %d)",
			bottom, m.ScrollOffset)
	}
	if m.scrollTarget != m.ScrollOffset {
		t.Errorf("scrollTarget out of sync after wheel-up: ScrollOffset=%d scrollTarget=%d "+
			"(ease will drag user back)", m.ScrollOffset, m.scrollTarget)
	}

	// One ease tick: must be a no-op since target == offset.
	before := m.ScrollOffset
	m.advanceScrollEase()
	if m.ScrollOffset != before {
		t.Errorf("ease tick moved ScrollOffset after user wheel-scroll: %d → %d",
			before, m.ScrollOffset)
	}
}

// TestArrowKey_DuringStreaming_DoesNotFightEase mirrors the wheel test
// for keyboard arrow scrolling — the same scrollTarget sync rule must
// hold so KeyUp/KeyDown don't get dragged back by the ease loop.
func TestArrowKey_DuringStreaming_DoesNotFightEase(t *testing.T) {
	m := beatPane()
	for range 50 {
		m.AppendText("line\n")
	}
	m.SetStatus(event.StatusThinking)
	m.spinnerRunning = true
	m.scrollToBottom()
	m.ScrollOffset = m.scrollTarget
	bottom := m.scrollTarget

	m.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})

	if m.ScrollOffset >= bottom {
		t.Fatalf("KeyUp did not move ScrollOffset (still %d)", m.ScrollOffset)
	}
	if m.scrollTarget != m.ScrollOffset {
		t.Errorf("scrollTarget out of sync after KeyUp: ScrollOffset=%d scrollTarget=%d",
			m.ScrollOffset, m.scrollTarget)
	}
}

// TestAdvanceScrollEase_SnapsWithinThreshold verifies the snap behavior:
// once ScrollOffset is within 2 lines of the target, the next ease
// tick lands exactly on the target. Catches a regression where the
// ease factor stalls at +/- 1 line.
func TestAdvanceScrollEase_SnapsWithinThreshold(t *testing.T) {
	m := beatPane()
	m.scrollTarget = 100

	// Set ScrollOffset within snap distance — a single ease call must
	// land exactly on the target.
	m.ScrollOffset = 99
	m.advanceScrollEase()
	if m.ScrollOffset != 100 {
		t.Errorf("close-to-target snap failed: ScrollOffset=%d; want 100", m.ScrollOffset)
	}

	// Same from the other direction.
	m.ScrollOffset = 101
	m.advanceScrollEase()
	if m.ScrollOffset != 100 {
		t.Errorf("close-from-above snap failed: ScrollOffset=%d; want 100", m.ScrollOffset)
	}
}
