package ui

import (
	"context"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// dispatchTestPane returns a freshly constructed AgentPaneModel
// suitable for dispatch tests. hasAgent=true mirrors the real
// startup wiring; commands don't read it, but the input pipeline
// is happier with the same shape production uses.
func dispatchTestPane() *AgentPaneModel {
	return NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
}

// fakeDispatchHandler records calls and optionally returns an
// error or queues a SubmitPrompt.
type fakeDispatchHandler struct {
	def     kitcmd.Definition
	calls   int
	err     error
	display string // if non-empty, Display this from Handle
	submit  string // if non-empty, SubmitPrompt this from Handle
	gotArgs string
}

func (f *fakeDispatchHandler) Definition() kitcmd.Definition { return f.def }
func (f *fakeDispatchHandler) Handle(ctx context.Context, sess kitcmd.Session, args string) error {
	f.calls++
	f.gotArgs = args
	if f.display != "" {
		sess.Display(f.display)
	}
	if f.submit != "" {
		_ = sess.SubmitPrompt(ctx, f.submit)
	}
	return f.err
}

func builtinDef(name string) kitcmd.Definition {
	return kitcmd.Definition{
		Name:        name,
		Description: "test",
		Source:      kitcmd.Source{Kind: kitcmd.SourceBuiltin, Path: "test"},
	}
}

func TestDispatchOrSubmit_NoRegistryFallsThrough(t *testing.T) {
	m := dispatchTestPane()
	cmd := m.dispatchOrSubmit("/help", false)
	if cmd == nil {
		t.Fatal("expected fallthrough cmd, got nil")
	}
	msg := cmd()
	gs, ok := msg.(GoalSubmittedMsg)
	if !ok {
		t.Fatalf("msg = %T, want GoalSubmittedMsg", msg)
	}
	if gs.Goal != "/help" {
		t.Errorf("Goal = %q, want %q", gs.Goal, "/help")
	}
}

func TestDispatchOrSubmit_NoRegistryHonorsPlanning(t *testing.T) {
	m := dispatchTestPane()
	cmd := m.dispatchOrSubmit("draft a plan", true)
	msg := cmd()
	if _, ok := msg.(PlanningGoalSubmittedMsg); !ok {
		t.Errorf("msg = %T, want PlanningGoalSubmittedMsg", msg)
	}
}

func TestDispatchOrSubmit_NonSlashFallsThrough(t *testing.T) {
	m := dispatchTestPane()
	r := kitcmd.NewRegistry()
	if err := r.Register(&fakeDispatchHandler{def: builtinDef("foo")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("regular goal", false)
	msg := cmd()
	if _, ok := msg.(GoalSubmittedMsg); !ok {
		t.Errorf("non-slash input should fall through to GoalSubmittedMsg; got %T", msg)
	}
}

func TestDispatchOrSubmit_SlashHandlerSwallowed(t *testing.T) {
	m := dispatchTestPane()
	h := &fakeDispatchHandler{def: builtinDef("foo")}
	r := kitcmd.NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("/foo bar", false)
	if cmd != nil {
		t.Errorf("matched dispatch should return nil cmd, got %T", cmd)
	}
	if h.calls != 1 || h.gotArgs != "bar" {
		t.Errorf("handler calls=%d args=%q, want 1, %q", h.calls, h.gotArgs, "bar")
	}
}

func TestDispatchOrSubmit_HandlerErrorRendersAndSwallows(t *testing.T) {
	m := dispatchTestPane()
	h := &fakeDispatchHandler{def: builtinDef("foo"), err: errStub("boom")}
	r := kitcmd.NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("/foo", false)
	if cmd != nil {
		t.Errorf("erroring dispatch should still swallow input; got cmd=%T", cmd)
	}
	// AppendMeta is called with the error message — verify it
	// surfaced into RawLines so the user sees it.
	joined := strings.Join(m.RawLines, "\n")
	if !strings.Contains(joined, "boom") {
		t.Errorf("RawLines should contain error %q; got %q", "boom", joined)
	}
}

func TestDispatchOrSubmit_HandlerErrorWithPendingPromptDoesNotSubmit(t *testing.T) {
	// A HandlerCommand that calls SubmitPrompt and then returns an
	// error has its prompt dropped — surfacing the error and ALSO
	// silently sending the prompt to the LLM would be a worst-of-both
	// outcome. Locks the doc contract: "Goal submission is suppressed
	// — the user typed a slash command, even if it errored."
	m := dispatchTestPane()
	h := &fakeDispatchHandler{
		def:    builtinDef("foo"),
		submit: "should not reach LLM",
		err:    errStub("template render failed"),
	}
	r := kitcmd.NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("/foo", false)
	if cmd != nil {
		t.Errorf("errored dispatch must drop pending prompt; got cmd=%T", cmd)
	}
	joined := strings.Join(m.RawLines, "\n")
	if !strings.Contains(joined, "template render failed") {
		t.Errorf("RawLines should contain error; got %q", joined)
	}
}

func TestDispatchOrSubmit_UnknownCommandReportsError(t *testing.T) {
	m := dispatchTestPane()
	r := kitcmd.NewRegistry()
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("/nope", false)
	if cmd != nil {
		t.Errorf("unknown command should be matched and swallowed, got cmd=%T", cmd)
	}
	joined := strings.Join(m.RawLines, "\n")
	if !strings.Contains(joined, "unknown command") {
		t.Errorf("expected 'unknown command' in RawLines, got %q", joined)
	}
}

func TestDispatchOrSubmit_BusyCheckReportsTurnInFlight(t *testing.T) {
	m := dispatchTestPane()
	r := kitcmd.NewRegistry()
	if err := r.Register(&fakeDispatchHandler{def: builtinDef("foo")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, func() bool { return true })

	cmd := m.dispatchOrSubmit("/foo", false)
	if cmd != nil {
		t.Errorf("busy dispatch should swallow input, got cmd=%T", cmd)
	}
	joined := strings.Join(m.RawLines, "\n")
	if !strings.Contains(joined, "turn in flight") {
		t.Errorf("expected 'turn in flight' in RawLines, got %q", joined)
	}
}

func TestDispatchOrSubmit_PromptSubmissionEmitsGoal(t *testing.T) {
	// A handler that calls SubmitPrompt (e.g. a markdown-style
	// prompt template) should cause dispatchOrSubmit to return a
	// Cmd that emits GoalSubmittedMsg with the rendered prompt —
	// matching the existing goal-submission flow.
	m := dispatchTestPane()
	h := &fakeDispatchHandler{def: builtinDef("foo"), submit: "rendered prompt text"}
	r := kitcmd.NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("/foo", false)
	if cmd == nil {
		t.Fatal("SubmitPrompt should produce a fallthrough cmd")
	}
	msg := cmd()
	gs, ok := msg.(GoalSubmittedMsg)
	if !ok {
		t.Fatalf("msg = %T, want GoalSubmittedMsg", msg)
	}
	if gs.Goal != "rendered prompt text" {
		t.Errorf("Goal = %q, want %q", gs.Goal, "rendered prompt text")
	}
}

func TestDispatchOrSubmit_PlanningSuppressedOnSlash(t *testing.T) {
	// Documented contract: a slash command does not carry the
	// planning bit forward. Even when the user typed in planning
	// mode, /foo dispatches as a normal command.
	m := dispatchTestPane()
	h := &fakeDispatchHandler{def: builtinDef("foo")}
	r := kitcmd.NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetCommandDispatch(context.Background(), r, nil)

	cmd := m.dispatchOrSubmit("/foo", true)
	if cmd != nil {
		t.Errorf("planning bit should not promote a slash cmd to planning submission; got %T", cmd)
	}
	if h.calls != 1 {
		t.Errorf("handler calls=%d, want 1", h.calls)
	}
}

// errStub is a tiny error type to avoid pulling in errors.New
// twice in the same file.
type errStub string

func (e errStub) Error() string { return string(e) }
