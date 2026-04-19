package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/junto/engine/event"
)

func newAwaitPane(t *testing.T) *AgentPaneModel {
	t.Helper()
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)
	m.SetSize(60, 24)
	return m
}

func sampleAwaitEvent() event.AgentAwaitingInput {
	return event.AgentAwaitingInput{
		Prompt: "Fix all or only new?",
		Reason: "Lint found pre-existing violations.",
		Options: []event.AwaitingInputOption{
			{ID: "fix-all", Label: "Fix every violation"},
			{ID: "fix-new", Label: "Only fix new ones"},
		},
		CallID: "call-1",
	}
}

func TestShowAwaitingInput_SetsStateAndRenders(t *testing.T) {
	m := newAwaitPane(t)

	if m.IsAwaitingInput() {
		t.Fatal("expected not awaiting initially")
	}

	m.ShowAwaitingInput(sampleAwaitEvent())

	if !m.IsAwaitingInput() {
		t.Error("expected IsAwaitingInput() true after ShowAwaitingInput")
	}
	if !m.IsInputActive() {
		t.Error("textarea should be focused after prompt appears")
	}

	// Prompt block must be in the transcript so it scrolls with content
	// and survives rewrap on resize.
	joined := strings.Join(m.RawLines, "\n")
	for _, want := range []string{
		"Agent needs your input",
		"Fix all or only new?",
		"Lint found pre-existing violations.",
		"fix-all",
		"Fix every violation",
		"fix-new",
		"Only fix new ones",
		"Esc cancels",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("prompt block missing %q\ngot:\n%s", want, joined)
		}
	}
}

func TestShowAwaitingInput_FreeFormOmitsOptionSection(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(event.AgentAwaitingInput{
		Prompt: "What module should this live in?",
	})

	joined := strings.Join(m.RawLines, "\n")
	if !strings.Contains(joined, "What module should this live in?") {
		t.Error("free-form prompt missing from transcript")
	}
	// No options means no " — " option separator should appear.
	if strings.Contains(joined, " — ") {
		t.Errorf("free-form prompt should not render option separator; got:\n%s", joined)
	}
}

func TestClearAwaitingInput_DropsState(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	m.ClearAwaitingInput()
	if m.IsAwaitingInput() {
		t.Error("ClearAwaitingInput should drop state")
	}
}

func TestClear_WipesAwaitingInput(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	m.Clear()
	if m.IsAwaitingInput() {
		t.Error("Clear() should wipe awaitingInput state")
	}
	if len(m.RawLines) != 0 {
		t.Errorf("Clear() should drop transcript; got %d lines", len(m.RawLines))
	}
}

func TestHandleInput_EnterAnswersPendingPrompt(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	m.input.SetContent("fix-all")

	cmd := m.handleInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a command from Enter")
	}
	msg := cmd()
	answered, ok := msg.(InputAnsweredMsg)
	if !ok {
		t.Fatalf("expected InputAnsweredMsg, got %T", msg)
	}
	if answered.Text != "fix-all" {
		t.Errorf("Text = %q, want fix-all", answered.Text)
	}
	if m.IsAwaitingInput() {
		t.Error("awaitingInput should be cleared optimistically on answer")
	}
}

func TestHandleInput_EnterSendsFreeFormVerbatim(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	m.input.SetContent("yes fix everything")

	cmd := m.handleInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a command from Enter")
	}
	answered, ok := cmd().(InputAnsweredMsg)
	if !ok {
		t.Fatalf("expected InputAnsweredMsg, got %T", cmd())
	}
	if answered.Text != "yes fix everything" {
		t.Errorf("Text = %q, want verbatim", answered.Text)
	}
}

func TestHandleInput_EnterWithEmptyContentIsNoOp(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	// Empty content; pressing Enter should drop the event and keep awaiting.
	cmd := m.handleInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		if _, ok := cmd().(InputAnsweredMsg); ok {
			t.Error("empty-content Enter should not emit InputAnsweredMsg")
		}
	}
}

func TestHandleInput_EscCancelsRunWhileAwaiting(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())

	cmd := m.handleInput(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("expected a command from Escape")
	}
	if _, ok := cmd().(CancelAgentMsg); !ok {
		t.Errorf("expected CancelAgentMsg while awaiting, got %T", cmd())
	}
	if m.IsAwaitingInput() {
		t.Error("awaitingInput should be cleared on Esc")
	}
}

func TestHandleInput_EscWithoutAwaitingJustDeactivates(t *testing.T) {
	m := newAwaitPane(t)
	m.SetInputActive(true)
	m.input.SetContent("some goal")

	cmd := m.handleInput(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		if _, ok := cmd().(CancelAgentMsg); ok {
			t.Error("Esc outside awaiting must not emit CancelAgentMsg")
		}
	}
	if m.IsInputActive() {
		t.Error("Esc should deactivate input when not awaiting")
	}
}

func TestStatusBar_AwaitingInputRendersDistinctly(t *testing.T) {
	m := newAwaitPane(t)
	m.SetStatus(event.StatusAwaitingInput)

	out := m.Render()
	if !strings.Contains(out, "Awaiting your answer") {
		t.Errorf("status bar should read 'Awaiting your answer' for StatusAwaitingInput; got:\n%s", out)
	}
	if !strings.Contains(out, "Esc cancel") {
		t.Errorf("status bar should show Esc hint; got:\n%s", out)
	}
}

func TestShowAwaitingInput_DropsPriorDraft(t *testing.T) {
	m := newAwaitPane(t)
	// Developer had typed a goal draft before the prompt appeared. It
	// must not become part of the answer to the new prompt.
	m.input.SetContent("previous goal draft")
	m.ShowAwaitingInput(sampleAwaitEvent())
	if m.input.Content() != "" {
		t.Errorf("ShowAwaitingInput must reset the textarea; got %q", m.input.Content())
	}
}

func TestClearAwaitingInput_DropsDraftAnswer(t *testing.T) {
	// A stale answer left in the textarea after the run exits would be
	// submitted as a brand-new goal the next time the input is focused.
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	m.input.SetContent("fix-a") // mid-typing draft
	m.ClearAwaitingInput()
	if m.input.Content() != "" {
		t.Errorf("ClearAwaitingInput must reset the textarea; got %q", m.input.Content())
	}
}

func TestClearAwaitingInput_WithNoPendingPromptIsNoOp(t *testing.T) {
	// If no prompt was pending, ClearAwaitingInput must not nuke a goal
	// draft the developer was composing.
	m := newAwaitPane(t)
	m.input.SetContent("a goal I'm writing")
	m.ClearAwaitingInput()
	if m.input.Content() != "a goal I'm writing" {
		t.Errorf("ClearAwaitingInput must leave unrelated drafts alone; got %q", m.input.Content())
	}
}

func TestHandleInput_EscWhileAwaitingDropsDraft(t *testing.T) {
	m := newAwaitPane(t)
	m.ShowAwaitingInput(sampleAwaitEvent())
	m.input.SetContent("fix-a")
	_ = m.handleInput(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.input.Content() != "" {
		t.Errorf("Esc while awaiting must reset textarea; got %q", m.input.Content())
	}
}

func TestRenderAwaitingInputBlock_PureFunction(t *testing.T) {
	got := renderAwaitingInputBlock(sampleAwaitEvent())
	for _, want := range []string{
		"Agent needs your input",
		"Fix all or only new?",
		"fix-all — Fix every violation",
		"fix-new — Only fix new ones",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in rendered block:\n%s", want, got)
		}
	}
}
