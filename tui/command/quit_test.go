package command

import (
	"context"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

func TestQuit_HandleInvokesQuitFn(t *testing.T) {
	calls := 0
	q := NewQuit(func() { calls++ })
	if err := q.Handle(context.Background(), NewPaneSession(nil), ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if calls != 1 {
		t.Errorf("quitFn calls = %d, want 1", calls)
	}
}

func TestQuit_HandleIgnoresArgs(t *testing.T) {
	// /quit with trailing args should still terminate; trailing
	// text is silently discarded rather than treated as an error.
	called := false
	q := NewQuit(func() { called = true })
	if err := q.Handle(context.Background(), NewPaneSession(nil), "now please"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !called {
		t.Errorf("quitFn was not called when args were present")
	}
}

func TestQuit_DefinitionShape(t *testing.T) {
	q := NewQuit(func() {})
	def := q.Definition()
	if def.Name != "quit" {
		t.Errorf("Name = %q, want quit", def.Name)
	}
	wantAliases := map[string]bool{"exit": true, "q": true}
	if len(def.Aliases) != len(wantAliases) {
		t.Errorf("Aliases = %v, want exit + q", def.Aliases)
	}
	for _, a := range def.Aliases {
		if !wantAliases[a] {
			t.Errorf("unexpected alias %q", a)
		}
	}
	if def.Source.Kind != kitcmd.SourceBuiltin {
		t.Errorf("Source.Kind = %v, want SourceBuiltin", def.Source.Kind)
	}
	if !strings.Contains(def.Description, "Exit") {
		t.Errorf("Description = %q, want it to mention Exit", def.Description)
	}
}

func TestNewQuit_NilFnPanics(t *testing.T) {
	// Fail-fast at construction: a /quit registered with a nil
	// callback would silently swallow the user's exit attempt,
	// trapping them in the TUI. Panic at New so the wiring bug
	// surfaces during the binary's startup, not at runtime.
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewQuit(nil) did not panic")
		}
	}()
	_ = NewQuit(nil)
}

func TestQuit_DispatchableThroughRegistry(t *testing.T) {
	// End-to-end smoke: a /quit registered in a kit Registry runs
	// when dispatched. Catches drift in HandlerCommand interface.
	calls := 0
	q := NewQuit(func() { calls++ })
	r := kitcmd.NewRegistry()
	if err := r.Register(q); err != nil {
		t.Fatalf("Register: %v", err)
	}
	matched, err := r.Dispatch(context.Background(), NewPaneSession(nil), "/quit")
	if !matched || err != nil {
		t.Fatalf("Dispatch matched=%v err=%v", matched, err)
	}
	if calls != 1 {
		t.Errorf("Handle calls = %d via registry, want 1", calls)
	}
	// And via alias.
	matched, err = r.Dispatch(context.Background(), NewPaneSession(nil), "/q")
	if !matched || err != nil {
		t.Fatalf("Dispatch via alias matched=%v err=%v", matched, err)
	}
	if calls != 2 {
		t.Errorf("Handle calls after alias = %d, want 2", calls)
	}
}
