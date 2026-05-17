package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/budget"
	kitcmd "github.com/latebit-io/nib/kit/command"
)

// fakeSnapshotter implements [ContextSnapshotter] for tests.
type fakeSnapshotter struct {
	snap  ContextSnapshot
	usage budget.Session
}

func (f fakeSnapshotter) EstimateContext() ContextSnapshot { return f.snap }
func (f fakeSnapshotter) Usage() budget.Session            { return f.usage }

// recordingCtxSession is a minimal kit.Session for capturing Display
// output. Tests assert against rendered text.
type recordingCtxSession struct {
	displays []string
}

func (r *recordingCtxSession) Display(text string) {
	r.displays = append(r.displays, text)
}

func (r *recordingCtxSession) SubmitPrompt(_ context.Context, _ string) error {
	return errors.New("recordingCtxSession: SubmitPrompt unexpected for /context")
}

var _ kitcmd.Session = (*recordingCtxSession)(nil)

func TestContext_RendersBreakdownAndTotals(t *testing.T) {
	snap := ContextSnapshot{
		Estimate: llm.InputEstimate{
			System:  1622,
			Tools:   5400,
			History: 24800,
			New:     180,
			Total:   32002,
		},
		ToolCount:    22,
		MessageCount: 152,
	}
	usage := budget.Session{
		TotalPromptTokens:     245100,
		TotalCompletionTokens: 18420,
		TotalCachedTokens:     198600,
		Turns:                 8,
	}
	c := NewContext(fakeSnapshotter{snap: snap, usage: usage})
	sess := &recordingCtxSession{}
	if err := c.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(sess.displays) != 1 {
		t.Fatalf("Display calls = %d, want 1", len(sess.displays))
	}
	out := sess.displays[0]
	for _, want := range []string{
		"Next request",
		"System prompt",
		"1,622",
		"Tool defs",
		"5,400",
		"22 tools",
		"History",
		"24,800",
		"152 messages",
		"New (pending)",
		"Total",
		"32,002",
		"Session totals (8 turn(s))",
		"Prompt",
		"245,100",
		"Completion",
		"18,420",
		"Cached",
		"198,600",
		"81%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Display output missing %q\nGot:\n%s", want, out)
		}
	}
}

func TestContext_NoTurnsYet(t *testing.T) {
	// Before any turn completes the session-totals section should say
	// so explicitly rather than printing a misleading 0/0 line.
	snap := ContextSnapshot{
		Estimate:     llm.InputEstimate{System: 1000, Tools: 3000, Total: 4000},
		ToolCount:    10,
		MessageCount: 1,
	}
	c := NewContext(fakeSnapshotter{snap: snap, usage: budget.Session{}})
	sess := &recordingCtxSession{}
	if err := c.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	out := sess.displays[0]
	if !strings.Contains(out, "no turns yet") {
		t.Errorf("expected no-turns notice, got:\n%s", out)
	}
	if strings.Contains(out, "0%") {
		t.Errorf("should not render cached %% before any turn, got:\n%s", out)
	}
}

func TestContext_OmitsNewLineWhenZero(t *testing.T) {
	// The "New (pending)" line is noise when nothing is pending —
	// suppress it so the breakdown stays tight.
	snap := ContextSnapshot{
		Estimate:     llm.InputEstimate{System: 100, Tools: 200, History: 300, New: 0, Total: 600},
		ToolCount:    2,
		MessageCount: 5,
	}
	c := NewContext(fakeSnapshotter{snap: snap})
	sess := &recordingCtxSession{}
	if err := c.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(sess.displays[0], "New (pending)") {
		t.Errorf("expected New line to be omitted when zero, got:\n%s", sess.displays[0])
	}
}

func TestContext_DefinitionShape(t *testing.T) {
	c := NewContext(fakeSnapshotter{})
	def := c.Definition()
	if def.Name != "context" {
		t.Errorf("Name = %q, want context", def.Name)
	}
	if def.Source.Kind != kitcmd.SourceBuiltin {
		t.Errorf("Source.Kind = %v, want SourceBuiltin", def.Source.Kind)
	}
	if !strings.Contains(def.Description, "token") {
		t.Errorf("Description = %q, want it to mention token", def.Description)
	}
}

func TestNewContext_NilSnapshotterPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewContext(nil) did not panic")
		}
	}()
	_ = NewContext(nil)
}

func TestContext_DispatchableThroughRegistry(t *testing.T) {
	c := NewContext(fakeSnapshotter{})
	r := kitcmd.NewRegistry()
	if err := r.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess := &recordingCtxSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/context")
	if !matched || err != nil {
		t.Fatalf("Dispatch matched=%v err=%v", matched, err)
	}
	if len(sess.displays) != 1 {
		t.Errorf("expected 1 Display call through registry, got %d", len(sess.displays))
	}
}

func TestFmtThousands(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{31822, "31,822"},
		{245100, "245,100"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, tc := range cases {
		if got := fmtThousands(tc.in); got != tc.want {
			t.Errorf("fmtThousands(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
