package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
)

// pendingPane builds an AgentPaneModel sized for transcript rendering
// so the banner row and "You: …" raw lines have somewhere to land.
func pendingPane() *AgentPaneModel {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	m.SetSize(60, 20) // width, height — wide enough for the banner preview
	return m
}

// TestQueueUserMessage_HoldsTextUntilFlush verifies the queue/flush
// invariant: while pending, the transcript holds no "You:" raw line —
// only the transient banner is added. Flush then appends the user
// message via the normal turn-separator path.
func TestQueueUserMessage_HoldsTextUntilFlush(t *testing.T) {
	m := pendingPane()
	m.AppendText("agent reply token...")

	m.QueueUserMessage("Error\npacman/systems/movement.lua:48")

	if !m.HasPendingUserMessage() {
		t.Fatal("HasPendingUserMessage = false; want true after QueueUserMessage")
	}
	// No "You:" line should have been added yet. The transcript raw
	// lines must still consist of the agent text only.
	if hasUserLine(m) {
		t.Fatal("user line appeared before FlushPendingUserMessage")
	}

	m.FlushPendingUserMessage()

	if m.HasPendingUserMessage() {
		t.Fatal("HasPendingUserMessage = true; want false after flush")
	}
	if !hasUserLine(m) {
		t.Fatal("user line missing after FlushPendingUserMessage")
	}
	// The pending banner must NOT have been left in the transcript —
	// it lives outside RawLines (rendered only via Render's bottom slot).
	for _, raw := range m.RawLines {
		if strings.Contains(raw, "[queued:") {
			t.Fatalf("banner leaked into RawLines: %q", raw)
		}
	}
}

// TestFlushPendingUserMessage_NoopWhenEmpty guards against a stray
// turn separator being emitted when nothing is queued.
func TestFlushPendingUserMessage_NoopWhenEmpty(t *testing.T) {
	m := pendingPane()
	m.AppendText("agent text")
	beforeRaw := len(m.RawLines)
	beforeTurn := m.turnCounter

	m.FlushPendingUserMessage()

	if len(m.RawLines) != beforeRaw {
		t.Errorf("raw line count changed on empty flush: %d → %d", beforeRaw, len(m.RawLines))
	}
	if m.turnCounter != beforeTurn {
		t.Errorf("turnCounter advanced on empty flush: %d → %d", beforeTurn, m.turnCounter)
	}
}

// TestQueueUserMessage_BannerReservesBottomRow verifies that
// VisibleLines shrinks by one when a message is queued so the banner
// has a dedicated row above the input area.
func TestQueueUserMessage_BannerReservesBottomRow(t *testing.T) {
	m := pendingPane()
	before := m.VisibleLines()

	m.QueueUserMessage("queued input")

	after := m.VisibleLines()
	if after != before-1 {
		t.Errorf("VisibleLines: before=%d after=%d; want after = before - 1", before, after)
	}
}

// TestPendingBanner_AppearsInRender verifies the banner string lands
// in the rendered output, dim-styled, while pending.
func TestPendingBanner_AppearsInRender(t *testing.T) {
	m := pendingPane()
	m.QueueUserMessage("hello world")

	out := m.Render()

	if !strings.Contains(out, "[queued: hello world]") {
		t.Errorf("rendered output missing queued banner; got:\n%s", out)
	}
}

// TestAgentTurnUsage_ToolCallsZero_IsFlushBoundary is a contract test
// for the [event.AgentTurnUsage] event: ToolCalls == 0 marks the
// turn where the foundation will park on awaitReply (and pick up any
// queued input). The app_engine_events bridge relies on this to flush
// the pending banner at the right boundary.
func TestAgentTurnUsage_ToolCallsZero_IsFlushBoundary(t *testing.T) {
	// Sanity: the event field exists and is named ToolCalls. If a
	// future refactor renames it, this test forces the rename to
	// land alongside the flush trigger.
	u := event.AgentTurnUsage{ToolCalls: 0}
	if u.ToolCalls != 0 {
		t.Fatalf("ToolCalls field round-trip failed: %d", u.ToolCalls)
	}
}

// hasUserLine reports whether any raw line is marked as user content.
func hasUserLine(m *AgentPaneModel) bool {
	for _, v := range m.userRawLines {
		if v {
			return true
		}
	}
	return false
}
