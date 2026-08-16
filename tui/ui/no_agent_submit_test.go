package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
)

// newNoAgentModel builds an AppModel whose Session has no agent wired —
// the first-run "No LLM configured" state.
func newNoAgentModel(t *testing.T) *AppModel {
	t.Helper()
	sess := session.New(openfile.New(buffer.New()), t.TempDir())
	m := &AppModel{
		Session:   sess,
		AgentPane: NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, false),
	}
	m.AgentPane.SetSize(80, 24)
	return m
}

// TestGoalSubmitted_NoAgent_SurfacesNotice locks the fix for Enter at the
// agent input silently dropping the goal when no provider is configured.
// Session.SubmitGoal returns false in that state, which the handler used to
// read as "new conversation started" and Clear() the pane.
func TestGoalSubmitted_NoAgent_SurfacesNotice(t *testing.T) {
	m := newNoAgentModel(t)
	m.AgentPane.AppendMeta("[earlier feedback]\n")

	m.handleGoalSubmitted(GoalSubmittedMsg{Goal: "do the thing"})

	out := m.AgentPane.Render()
	if !strings.Contains(out, "not sent: no LLM configured") {
		t.Fatalf("no-agent submit must surface an inline notice; got:\n%s", out)
	}
	if !strings.Contains(out, "[earlier feedback]") {
		t.Fatalf("no-agent submit must not clear prior transcript feedback; got:\n%s", out)
	}
	if m.Session.CurrentIntent() != "" {
		t.Fatalf("no-agent submit must not start an intent; got %q", m.Session.CurrentIntent())
	}
}

// TestPlanningGoalSubmitted_NoAgent_SurfacesNotice covers the planning
// submission path, which shares the silent no-op with the regular one.
func TestPlanningGoalSubmitted_NoAgent_SurfacesNotice(t *testing.T) {
	m := newNoAgentModel(t)

	m.handlePlanningGoalSubmitted(PlanningGoalSubmittedMsg{Goal: "plan the thing"})

	if out := m.AgentPane.Render(); !strings.Contains(out, "not sent: no LLM configured") {
		t.Fatalf("no-agent planning submit must surface an inline notice; got:\n%s", out)
	}
}

// TestRender_NoAgent_ShowsTranscriptTail locks the splash rendering meta
// feedback: key-save failures, OAuth instructions and slash-command output
// all land via AppendMeta and were invisible until an agent existed.
func TestRender_NoAgent_ShowsTranscriptTail(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, false)
	m.SetSize(60, 8)
	for i := range 20 {
		m.AppendMeta("[line " + string(rune('a'+i)) + "]\n")
	}

	out := m.Render()
	if !strings.Contains(out, "No LLM configured") {
		t.Fatalf("splash header missing; got:\n%s", out)
	}
	if !strings.Contains(out, "[line t]") {
		t.Fatalf("splash must show the transcript tail; got:\n%s", out)
	}
	if strings.Contains(out, "[line a]") {
		t.Fatalf("splash must show only the tail that fits; got:\n%s", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 8 {
		t.Fatalf("render height = %d, want 8", got)
	}

	// The API-key overlay still paints in the band below the tail.
	m.StartAPIKeyInput("fugu")
	out = m.Render()
	if !strings.Contains(out, "API key for fugu") {
		t.Fatalf("overlay must still render over the tail; got:\n%s", out)
	}
	if got := strings.Count(out, "\n") + 1; got != 8 {
		t.Fatalf("render height with overlay = %d, want 8", got)
	}
}

// TestAPIKeyEscape_NoAgent_DoesNotFocusInput: the splash does not draw the
// textarea, so closing the overlay must not hand focus to an invisible input.
func TestAPIKeyEscape_NoAgent_DoesNotFocusInput(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, false)
	m.SetSize(60, 24)
	m.StartAPIKeyInput("fugu")

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.IsInputActive() {
		t.Fatal("no-agent overlay close must not activate the hidden textarea")
	}

	m.SetHasAgent(true)
	m.StartAPIKeyInput("fugu")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.IsInputActive() {
		t.Fatal("with an agent, overlay close should return focus to the textarea")
	}
}
