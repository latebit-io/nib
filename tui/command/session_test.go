package command

import (
	"context"
	"testing"
)

type fakePane struct {
	metas []string
}

func (f *fakePane) AppendMeta(text string) {
	f.metas = append(f.metas, text)
}

func TestPaneSession_DisplayRoutesToPane(t *testing.T) {
	p := &fakePane{}
	s := NewPaneSession(p)
	s.Display("hello")
	s.Display("world")
	if got := p.metas; len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Errorf("metas = %v, want [hello world]", got)
	}
}

func TestPaneSession_DisplayNilPaneIsNoop(t *testing.T) {
	// nil pane is a documented no-op so tests that only exercise
	// SubmitPrompt do not need a fake.
	s := NewPaneSession(nil)
	s.Display("anything") // must not panic
}

func TestPaneSession_PendingPromptUnsetByDefault(t *testing.T) {
	s := NewPaneSession(nil)
	if text, ok := s.PendingPrompt(); ok || text != "" {
		t.Errorf("PendingPrompt = (%q, %v), want (\"\", false)", text, ok)
	}
}

func TestPaneSession_SubmitPromptRecords(t *testing.T) {
	s := NewPaneSession(nil)
	if err := s.SubmitPrompt(context.Background(), "do the thing"); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	text, ok := s.PendingPrompt()
	if !ok || text != "do the thing" {
		t.Errorf("PendingPrompt = (%q, %v), want (\"do the thing\", true)", text, ok)
	}
}

func TestPaneSession_SubmitPromptOverwrites(t *testing.T) {
	// Documented: a second SubmitPrompt overwrites the first. Pin
	// the behavior so a future refactor that decides to merge
	// instead breaks this test.
	s := NewPaneSession(nil)
	if err := s.SubmitPrompt(context.Background(), "first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.SubmitPrompt(context.Background(), "second"); err != nil {
		t.Fatalf("second: %v", err)
	}
	text, ok := s.PendingPrompt()
	if !ok || text != "second" {
		t.Errorf("PendingPrompt = (%q, %v), want (\"second\", true)", text, ok)
	}
}

func TestPaneSession_DisplayAndSubmitIndependent(t *testing.T) {
	// Display does not affect PendingPrompt; SubmitPrompt does
	// not write to the pane.
	p := &fakePane{}
	s := NewPaneSession(p)
	s.Display("status")
	if _, ok := s.PendingPrompt(); ok {
		t.Errorf("Display should not set PendingPrompt")
	}
	if err := s.SubmitPrompt(context.Background(), "go"); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	if len(p.metas) != 1 || p.metas[0] != "status" {
		t.Errorf("SubmitPrompt should not call Display; metas=%v", p.metas)
	}
}
