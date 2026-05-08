package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

type fakeResetter struct {
	calls int
	err   error
}

func (f *fakeResetter) ResetHistory(_ context.Context) error {
	f.calls++
	return f.err
}

func TestClear_HandleResetsHistoryAndClearsPane(t *testing.T) {
	r := &fakeResetter{}
	p := &fakePane{}
	c := NewClear(r, p)
	if err := c.Handle(context.Background(), NewPaneSession(p), ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r.calls != 1 {
		t.Errorf("ResetHistory calls = %d, want 1", r.calls)
	}
	if p.clears != 1 {
		t.Errorf("pane.Clear calls = %d, want 1", p.clears)
	}
}

func TestClear_HandleDoesNotClearPaneOnResetFailure(t *testing.T) {
	// Surfacing the error against an intact transcript is more
	// useful than leaving the user with a blank pane and no signal
	// of what went wrong.
	want := errors.New("boom")
	r := &fakeResetter{err: want}
	p := &fakePane{}
	c := NewClear(r, p)
	if err := c.Handle(context.Background(), NewPaneSession(p), ""); !errors.Is(err, want) {
		t.Fatalf("Handle err = %v, want %v", err, want)
	}
	if p.clears != 0 {
		t.Errorf("pane.Clear should not run on reset failure; clears = %d", p.clears)
	}
}

func TestClear_DefinitionShape(t *testing.T) {
	c := NewClear(&fakeResetter{}, &fakePane{})
	def := c.Definition()
	if def.Name != "clear" {
		t.Errorf("Name = %q, want clear", def.Name)
	}
	if def.Source.Kind != kitcmd.SourceBuiltin {
		t.Errorf("Source.Kind = %v, want SourceBuiltin", def.Source.Kind)
	}
	if !strings.Contains(strings.ToLower(def.Description), "clear") {
		t.Errorf("Description = %q, want it to mention clear", def.Description)
	}
}

func TestNewClear_NilArgsPanic(t *testing.T) {
	tests := []struct {
		name     string
		resetter HistoryResetter
		pane     Pane
	}{
		{"nil resetter", nil, &fakePane{}},
		{"nil pane", &fakeResetter{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewClear with %s did not panic", tt.name)
				}
			}()
			_ = NewClear(tt.resetter, tt.pane)
		})
	}
}

func TestClear_DispatchableThroughRegistry(t *testing.T) {
	r := &fakeResetter{}
	p := &fakePane{}
	c := NewClear(r, p)
	reg := kitcmd.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	matched, err := reg.Dispatch(context.Background(), NewPaneSession(p), "/clear")
	if !matched || err != nil {
		t.Fatalf("Dispatch matched=%v err=%v", matched, err)
	}
	if r.calls != 1 || p.clears != 1 {
		t.Errorf("after registry dispatch: ResetHistory=%d pane.Clear=%d, want 1/1", r.calls, p.clears)
	}
}
