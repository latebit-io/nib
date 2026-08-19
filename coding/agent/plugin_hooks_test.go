package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/pluginhooks"
)

// fakeHookDispatcher is a configurable HookDispatcher for wiring tests —
// it records what the gates pass and returns the verdicts it is told to.
type fakeHookDispatcher struct {
	preDeny    bool
	preReason  string
	postDeny   bool
	postReason string
	promptDeny bool
	promptMsg  string

	// postCustom returns postContent/postIsErr verbatim, to exercise
	// content-only / error-only overrides the v1 dispatcher never emits.
	postCustom  bool
	postContent *string
	postIsErr   *bool

	sawPreTool, sawPreArgs string
	sawPostResult          string
	sawPrompt              string

	sessionStarts int
	stops         int
	preCompacts   int
}

func (f *fakeHookDispatcher) PreToolUse(_ context.Context, tool, args string) pluginhooks.Decision {
	f.sawPreTool, f.sawPreArgs = tool, args
	if f.preDeny {
		return pluginhooks.Decision{Deny: true, Reason: f.preReason}
	}
	return pluginhooks.Decision{}
}

func (f *fakeHookDispatcher) PostToolUse(_ context.Context, _, _, resultText string) (*string, *bool) {
	f.sawPostResult = resultText
	if f.postCustom {
		return f.postContent, f.postIsErr
	}
	if f.postDeny {
		reason, isErr := f.postReason, true
		return &reason, &isErr
	}
	return nil, nil
}

func (f *fakeHookDispatcher) UserPromptSubmit(_ context.Context, prompt string) pluginhooks.Decision {
	f.sawPrompt = prompt
	if f.promptDeny {
		return pluginhooks.Decision{Deny: true, Reason: f.promptMsg}
	}
	return pluginhooks.Decision{}
}
func (f *fakeHookDispatcher) SessionStart(context.Context) { f.sessionStarts++ }
func (f *fakeHookDispatcher) Stop(context.Context)         { f.stops++ }
func (f *fakeHookDispatcher) PreCompact(context.Context)   { f.preCompacts++ }

func TestPluginPreToolUseGate_DenyBlocks(t *testing.T) {
	fake := &fakeHookDispatcher{preDeny: true, preReason: "plugin says no"}
	a := &Agent{hooks: fake}
	res := a.pluginPreToolUseGate(context.Background(),
		upagent.BeforeToolCallInput{Name: "write_file", Args: `{"path":"x"}`})
	if !res.Block || res.Reason != "plugin says no" {
		t.Fatalf("gate = %+v, want Block with reason %q", res, "plugin says no")
	}
	// The gate passes the dispatched tool name and raw args through.
	if fake.sawPreTool != "write_file" || fake.sawPreArgs != `{"path":"x"}` {
		t.Fatalf("dispatcher saw (%q, %q), want (write_file, {\"path\":\"x\"})", fake.sawPreTool, fake.sawPreArgs)
	}
}

func TestPluginPreToolUseGate_ProceedDoesNotBlock(t *testing.T) {
	a := &Agent{hooks: &fakeHookDispatcher{}}
	if res := a.pluginPreToolUseGate(context.Background(),
		upagent.BeforeToolCallInput{Name: "bash"}); res.Block {
		t.Fatalf("gate = %+v, want no block", res)
	}
}

func TestPluginPreToolUseGate_NilDispatcherProceeds(t *testing.T) {
	a := &Agent{} // hooks nil — directly-built agent (and the no-plugin case)
	if res := a.pluginPreToolUseGate(context.Background(),
		upagent.BeforeToolCallInput{Name: "write_file"}); res.Block {
		t.Fatalf("nil-dispatcher gate = %+v, want no block", res)
	}
}

func TestPluginPostToolUse_DenyFlipsToError(t *testing.T) {
	fake := &fakeHookDispatcher{postDeny: true, postReason: "bad output"}
	a := &Agent{hooks: fake}
	in := upagent.AfterToolCallInput{
		Name:   "bash",
		Args:   `{}`,
		Result: upagent.ToolResult{Content: "raw tool output", IsError: false},
	}
	res := a.pluginPostToolUse(context.Background(), in, upagent.AfterToolCallResult{})
	if res.IsError == nil || !*res.IsError {
		t.Fatalf("IsError = %v, want true", res.IsError)
	}
	if res.Content == nil || *res.Content != "bad output" {
		t.Fatalf("Content = %v, want %q", res.Content, "bad output")
	}
	// The hook must see the tool's ORIGINAL output, not a prior override.
	if fake.sawPostResult != "raw tool output" {
		t.Fatalf("dispatcher saw result %q, want %q", fake.sawPostResult, "raw tool output")
	}
}

func TestPluginPostToolUse_ProceedLeavesResultUntouched(t *testing.T) {
	a := &Agent{hooks: &fakeHookDispatcher{}}
	prior := "lifecycle trailer"
	in := upagent.AfterToolCallInput{Name: "edit_file", Result: upagent.ToolResult{Content: "out"}}
	res := a.pluginPostToolUse(context.Background(), in, upagent.AfterToolCallResult{Content: &prior})
	if res.IsError != nil {
		t.Fatalf("IsError = %v, want nil (untouched)", res.IsError)
	}
	if res.Content != &prior {
		t.Fatalf("Content pointer changed; want the prior override preserved on proceed")
	}
}

func TestPluginPostToolUse_ContentAndErrorOverridesAreIndependent(t *testing.T) {
	// A content-only override (isError nil) must still apply — the two
	// override fields are independent.
	rewritten := "rewritten output"
	a := &Agent{hooks: &fakeHookDispatcher{postCustom: true, postContent: &rewritten}}
	res := a.pluginPostToolUse(context.Background(),
		upagent.AfterToolCallInput{Name: "bash", Result: upagent.ToolResult{Content: "orig"}},
		upagent.AfterToolCallResult{})
	if res.Content == nil || *res.Content != rewritten {
		t.Fatalf("Content = %v, want content-only override applied", res.Content)
	}
	if res.IsError != nil {
		t.Fatalf("IsError = %v, want nil (no error override given)", res.IsError)
	}

	// An error-only override (content nil) flips IsError without touching content.
	isErr := true
	a = &Agent{hooks: &fakeHookDispatcher{postCustom: true, postIsErr: &isErr}}
	res = a.pluginPostToolUse(context.Background(),
		upagent.AfterToolCallInput{Name: "bash"}, upagent.AfterToolCallResult{})
	if res.IsError == nil || !*res.IsError {
		t.Fatalf("IsError = %v, want true", res.IsError)
	}
	if res.Content != nil {
		t.Fatalf("Content = %v, want nil (no content override given)", res.Content)
	}
}

func TestPluginPostToolUse_NilDispatcherUntouched(t *testing.T) {
	a := &Agent{}
	res := a.pluginPostToolUse(context.Background(),
		upagent.AfterToolCallInput{Name: "bash"}, upagent.AfterToolCallResult{})
	if res.IsError != nil || res.Content != nil {
		t.Fatalf("nil-dispatcher result = %+v, want zero", res)
	}
}

// TestNew_PlumbsHookDispatcher verifies NewOptions.HookDispatcher reaches
// Agent.hooks (the construction wiring), distinct from the gate-logic
// tests above which set the field directly.
func TestNew_PlumbsHookDispatcher(t *testing.T) {
	fake := &fakeHookDispatcher{preDeny: true, preReason: "wired"}
	ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{HookDispatcher: fake})
	t.Cleanup(ag.Close)
	res := ag.pluginPreToolUseGate(context.Background(),
		upagent.BeforeToolCallInput{Name: "write_file"})
	if !res.Block || res.Reason != "wired" {
		t.Fatalf("New-wired gate = %+v, want Block reason %q", res, "wired")
	}
}

func TestPluginLifecycleHelpers_Delegate(t *testing.T) {
	fake := &fakeHookDispatcher{}
	a := &Agent{hooks: fake}
	ctx := context.Background()
	a.pluginSessionStart(ctx)
	a.pluginStop(ctx)
	a.pluginPreCompact(ctx)
	if fake.sessionStarts != 1 || fake.stops != 1 || fake.preCompacts != 1 {
		t.Fatalf("delegation counts = %d/%d/%d, want 1/1/1",
			fake.sessionStarts, fake.stops, fake.preCompacts)
	}
	if dec := a.pluginUserPromptSubmit(ctx, "hi"); dec.Deny {
		t.Fatalf("proceed prompt = %+v", dec)
	}
	if fake.sawPrompt != "hi" {
		t.Fatalf("prompt passthrough = %q, want %q", fake.sawPrompt, "hi")
	}
}

func TestPluginLifecycleHelpers_NilSafe(t *testing.T) {
	a := &Agent{} // hooks nil
	ctx := context.Background()
	a.pluginSessionStart(ctx)
	a.pluginStop(ctx)
	a.pluginPreCompact(ctx)
	if dec := a.pluginUserPromptSubmit(ctx, "x"); dec.Deny {
		t.Fatalf("nil-dispatcher prompt = %+v, want proceed", dec)
	}
}

func TestPromptBlockedReason(t *testing.T) {
	const generic = "Prompt blocked by a plugin hook."
	if got := promptBlockedReason(""); got != generic {
		t.Errorf("empty reason = %q, want generic", got)
	}
	if got := promptBlockedReason("   "); got != generic {
		t.Errorf("blank reason = %q, want generic", got)
	}
	if got := promptBlockedReason("nope"); got != "nope" {
		t.Errorf("reason = %q, want passthrough", got)
	}
}

func TestRunWithMode_UserPromptSubmitDenyBlocksRun(t *testing.T) {
	fake := &fakeHookDispatcher{promptDeny: true, promptMsg: "no goals allowed"}
	ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{HookDispatcher: fake})
	t.Cleanup(ag.Close)
	ch := subscribeForTest(t, ag)

	ag.RunWithMode(context.Background(), "f.go", "code", "do the thing", event.ModeExecution)

	if drainUntil(t, ch, 2*time.Second, func(ev event.Event) bool {
		e, ok := ev.(event.AgentError)
		return ok && strings.Contains(e.Err, "no goals allowed")
	}) == nil {
		t.Fatal("expected AgentError carrying the deny reason")
	}
	if fake.sessionStarts != 1 {
		t.Errorf("SessionStart fired %d times, want 1 (per conversation)", fake.sessionStarts)
	}
	if fake.sawPrompt != "do the thing" {
		t.Errorf("UserPromptSubmit saw %q, want the goal", fake.sawPrompt)
	}
	if ag.IsRunning() {
		t.Error("run must not be active after a denied prompt")
	}
}

func TestReply_UserPromptSubmitDenyBlocks(t *testing.T) {
	fake := &fakeHookDispatcher{promptDeny: true, promptMsg: "blocked reply"}
	ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{HookDispatcher: fake})
	t.Cleanup(ag.Close)
	ch := subscribeForTest(t, ag)

	// No active run / no saved transcript: a proceeding Reply returns
	// false. The deny path returns true (handled) without resuming.
	if !ag.Reply(context.Background(), "please do X") {
		t.Fatal("denied Reply should return true (handled, not a no-conversation false)")
	}
	if drainUntil(t, ch, 2*time.Second, func(ev event.Event) bool {
		e, ok := ev.(event.AgentError)
		return ok && strings.Contains(e.Err, "blocked reply")
	}) == nil {
		t.Fatal("expected AgentError carrying the deny reason")
	}
	if fake.sawPrompt != "please do X" {
		t.Errorf("UserPromptSubmit saw %q, want the reply input", fake.sawPrompt)
	}
}

func TestReply_NoConversationReturnsFalse(t *testing.T) {
	ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{HookDispatcher: &fakeHookDispatcher{}})
	t.Cleanup(ag.Close)
	_ = subscribeForTest(t, ag)
	if ag.Reply(context.Background(), "x") {
		t.Fatal("Reply with no conversation and no deny should return false")
	}
}
