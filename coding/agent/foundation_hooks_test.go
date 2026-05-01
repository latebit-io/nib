package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/budget"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/nudges"
)

// Foundation hook tests cover each gate in isolation, then exercise
// the composed Hooks value to verify the in-order dispatch + the
// closure-captured turn state.

// --- planningBlocklistGate ---

func TestPlanningBlocklistGate_BlocksInPlanningMode(t *testing.T) {
	a := &Agent{
		mode:              event.ModePlanning,
		planningBlocklist: map[string]bool{"edit_file": true},
	}

	res := a.planningBlocklistGate(upagent.BeforeToolCallContext{Name: "edit_file"})
	if !res.Block {
		t.Fatalf("expected Block=true for blocklisted tool in planning mode")
	}
	if !strings.Contains(res.Reason, "edit_file") || !strings.Contains(res.Reason, "planning mode") {
		t.Errorf("unexpected reason: %q", res.Reason)
	}
}

func TestPlanningBlocklistGate_CaseInsensitive(t *testing.T) {
	a := &Agent{
		mode:              event.ModePlanning,
		planningBlocklist: map[string]bool{"edit_file": true},
	}
	res := a.planningBlocklistGate(upagent.BeforeToolCallContext{Name: "Edit_File"})
	if !res.Block {
		t.Fatalf("expected Block for case-different name")
	}
}

func TestPlanningBlocklistGate_AllowsNonBlocklisted(t *testing.T) {
	a := &Agent{
		mode:              event.ModePlanning,
		planningBlocklist: map[string]bool{"edit_file": true},
	}
	res := a.planningBlocklistGate(upagent.BeforeToolCallContext{Name: "read_file"})
	if res.Block {
		t.Errorf("expected non-block for tool off the blocklist; reason=%q", res.Reason)
	}
}

func TestPlanningBlocklistGate_OffInExecution(t *testing.T) {
	a := &Agent{
		mode:              event.ModeExecution,
		planningBlocklist: map[string]bool{"edit_file": true},
	}
	res := a.planningBlocklistGate(upagent.BeforeToolCallContext{Name: "edit_file"})
	if res.Block {
		t.Errorf("planning-mode blocklist must not fire in execution mode")
	}
}

// --- activeTaskGate ---

func TestActiveTaskGate_BlocksWhenNoActiveTask(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: ""}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	a := &Agent{mode: event.ModeExecution, workspace: ws}

	res := a.activeTaskGate(context.Background(), upagent.BeforeToolCallContext{Name: "edit_file"})
	if !res.Block {
		t.Fatalf("expected Block when no active task")
	}
	if !strings.Contains(res.Reason, "no active task") {
		t.Errorf("expected active-task error, got: %q", res.Reason)
	}
}

func TestActiveTaskGate_AllowsWhenActiveTaskSet(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: "Phase 1 > task"}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	a := &Agent{mode: event.ModeExecution, workspace: ws}

	res := a.activeTaskGate(context.Background(), upagent.BeforeToolCallContext{Name: "edit_file"})
	if res.Block {
		t.Errorf("expected non-block with active task; reason=%q", res.Reason)
	}
}

// --- singleEditGate ---

func TestSingleEditGate_FirstEditPasses(t *testing.T) {
	a := &Agent{interactionMode: Interactive, autonomous: false}
	fired := false

	res := a.singleEditGate(upagent.BeforeToolCallContext{Name: "edit_file"}, &fired)
	if res.Block {
		t.Fatalf("first edit must not be blocked; reason=%q", res.Reason)
	}
	if !fired {
		t.Errorf("expected firedThisTurn=true after first file-edit")
	}
}

func TestSingleEditGate_SecondEditBlocked(t *testing.T) {
	a := &Agent{interactionMode: Interactive, autonomous: false}
	fired := true // already-fired state

	res := a.singleEditGate(upagent.BeforeToolCallContext{Name: "edit_file"}, &fired)
	if !res.Block {
		t.Fatalf("second edit must be blocked")
	}
	if !strings.Contains(res.Reason, "ONE file-edit per turn") {
		t.Errorf("expected one-edit message, got: %q", res.Reason)
	}
}

func TestSingleEditGate_HeadlessDoesNotEnforce(t *testing.T) {
	a := &Agent{interactionMode: Headless, autonomous: false}
	fired := true

	res := a.singleEditGate(upagent.BeforeToolCallContext{Name: "edit_file"}, &fired)
	if res.Block {
		t.Errorf("headless mode must not enforce single-edit; reason=%q", res.Reason)
	}
}

func TestSingleEditGate_AutonomousDoesNotEnforce(t *testing.T) {
	a := &Agent{interactionMode: Interactive, autonomous: true}
	fired := true

	res := a.singleEditGate(upagent.BeforeToolCallContext{Name: "edit_file"}, &fired)
	if res.Block {
		t.Errorf("autonomous mode must not enforce single-edit; reason=%q", res.Reason)
	}
}

func TestSingleEditGate_NonEditToolPasses(t *testing.T) {
	// bash and smoke_run are mutating but NOT file-edit tools — they may
	// chain after an edit (e.g. "edit then go test").
	a := &Agent{interactionMode: Interactive, autonomous: false}
	fired := true

	for _, tool := range []string{"bash", "smoke_run", "read_file"} {
		res := a.singleEditGate(upagent.BeforeToolCallContext{Name: tool}, &fired)
		if res.Block {
			t.Errorf("%s should not be gated by single-edit", tool)
		}
	}
}

// --- FoundationHooks() composed behavior ---

func TestFoundationHooks_BeforeToolCallChain_Order(t *testing.T) {
	// Single-edit fires first in inline; verify the composed chain
	// preserves that ordering. With singleEditFired pre-flipped to true
	// AND a planning-mode blocklist match, the single-edit message wins.
	a := &Agent{
		mode:              event.ModePlanning,
		planningBlocklist: map[string]bool{"edit_file": true},
		interactionMode:   Interactive,
		autonomous:        false,
	}
	hooks := a.FoundationHooks(nil)
	// Fire one edit to flip closure state.
	if _, err := hooks.BeforeToolCall(context.Background(), upagent.BeforeToolCallContext{Name: "edit_file"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call should hit single-edit, not planning-blocklist.
	res, err := hooks.BeforeToolCall(context.Background(), upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !res.Block {
		t.Fatal("expected Block on second edit")
	}
	if !strings.Contains(res.Reason, "ONE file-edit per turn") {
		t.Errorf("expected single-edit message to win; got: %q", res.Reason)
	}
}

func TestFoundationHooks_TransformContextResetsTurnState(t *testing.T) {
	// One file-edit per turn. After TransformContext fires, the next
	// turn's first edit should pass.
	a := &Agent{
		events:          mustDrainEvents(t),
		cache:           NewFileCache(),
		mode:            event.ModeExecution,
		interactionMode: Interactive,
		autonomous:      false,
	}
	hooks := a.FoundationHooks(nil)
	ctx := context.Background()

	// Turn 1: first edit passes, second blocked.
	res1, err := hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("turn 1 first BeforeToolCall: %v", err)
	}
	if res1.Block {
		t.Fatalf("turn 1 first edit should pass; reason=%q", res1.Reason)
	}
	res2, err := hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("turn 1 second BeforeToolCall: %v", err)
	}
	if !res2.Block {
		t.Fatalf("turn 1 second edit should be blocked")
	}

	// TransformContext fires at the top of turn 2 — must reset.
	if _, err := hooks.TransformContext(ctx, nil); err != nil {
		t.Fatalf("TransformContext: %v", err)
	}

	// Turn 2: first edit passes again.
	res3, err := hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("turn 2 BeforeToolCall: %v", err)
	}
	if res3.Block {
		t.Errorf("turn 2 first edit should pass after reset; reason=%q", res3.Reason)
	}
}

func TestFoundationHooks_MigratedHooksPresent(t *testing.T) {
	// Pin the wiring contract so future commits notice when the
	// migrated set changes. After the 8c cutover every hook the
	// foundation exposes is wired by the application — the bare
	// foundation has nothing application-specific to layer on, so
	// missing wiring would silently drop a behavior.
	a := &Agent{}
	hooks := a.FoundationHooks(nil)
	if hooks.BeforeToolCall == nil {
		t.Errorf("BeforeToolCall must be wired (concerns #1, #2 + flushDirtyBuffers + AgentToolCall emit)")
	}
	if hooks.TransformContext == nil {
		t.Errorf("TransformContext must be wired (concerns #2, #3, #6 + system prompt rebuild + EstimateAndBroadcast)")
	}
	if hooks.GetSteeringMessages == nil {
		t.Errorf("GetSteeringMessages must be wired (concern #4)")
	}
	if hooks.AfterToolCall == nil {
		t.Errorf("AfterToolCall must be wired (concern #5)")
	}
	if hooks.GetFollowUpMessages == nil {
		t.Errorf("GetFollowUpMessages must be wired — drives AgentWaiting emission at the loop-park boundary")
	}
	if hooks.OnTruncated == nil {
		t.Errorf("OnTruncated must be wired — preserves the truncation-recovery contract from the inline path")
	}
}

// --- foundationCompactAndLint ---

func TestFoundationCompactAndLint_NoLintNoCompactionPasses(t *testing.T) {
	// Small transcript, no pending lint — TransformContext returns
	// the slice untouched (modulo MaybeCompact's short-circuit).
	a := &Agent{}
	msgs := []llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	out, err := a.foundationCompactAndLint(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected unchanged length %d, got %d", len(msgs), len(out))
	}
}

func TestFoundationCompactAndLint_InjectsLintMessage(t *testing.T) {
	a := &Agent{pendingLint: "vet: declared but not used: foo"}
	msgs := []llm.Message{{Role: "user", Content: "fix it"}}

	out, err := a.foundationCompactAndLint(context.Background(), msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != len(msgs)+1 {
		t.Fatalf("expected one appended message, got len=%d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != "user" {
		t.Errorf("expected appended message Role=user, got %q", last.Role)
	}
	if !strings.Contains(last.Content, "STOP") || !strings.Contains(last.Content, "lint") {
		t.Errorf("expected lint preamble in appended content, got: %q", last.Content)
	}
	if !strings.Contains(last.Content, "declared but not used") {
		t.Errorf("expected lint output to be quoted into appended content, got: %q", last.Content)
	}
	// Drain semantics: pendingLint must be cleared after the call.
	if a.pendingLint != "" {
		t.Errorf("expected pendingLint to be drained, got %q", a.pendingLint)
	}
}

func TestFoundationCompactAndLint_DrainOnlyOnce(t *testing.T) {
	// Calling TransformContext twice in succession (e.g. retry path)
	// must not re-inject the same lint preamble.
	a := &Agent{pendingLint: "some violation"}
	msgs := []llm.Message{{Role: "user", Content: "."}}

	out1, err := a.foundationCompactAndLint(context.Background(), msgs)
	if err != nil {
		t.Fatalf("first foundationCompactAndLint: %v", err)
	}
	if len(out1) != 2 {
		t.Fatalf("first call: expected len=2, got %d", len(out1))
	}
	out2, err := a.foundationCompactAndLint(context.Background(), msgs)
	if err != nil {
		t.Fatalf("second foundationCompactAndLint: %v", err)
	}
	if len(out2) != 1 {
		t.Errorf("second call: expected drained pendingLint to leave msgs unchanged, len=%d", len(out2))
	}
}

// --- activeToolDefs ---

func TestActiveToolDefs_PlanningFiltersBlocklist(t *testing.T) {
	defs := []llm.ToolDef{
		{Function: llm.FunctionDef{Name: "read_file"}},
		{Function: llm.FunctionDef{Name: "edit_file"}},
		{Function: llm.FunctionDef{Name: "bash"}},
	}
	a := &Agent{
		mode:              event.ModePlanning,
		toolDefs:          defs,
		planningBlocklist: map[string]bool{"edit_file": true, "bash": true},
	}
	got := a.activeToolDefs()
	if len(got) != 1 {
		t.Fatalf("expected 1 tool after planning filter, got %d", len(got))
	}
	if got[0].Function.Name != "read_file" {
		t.Errorf("expected read_file, got %q", got[0].Function.Name)
	}
}

func TestActiveToolDefs_ExecutionReturnsAll(t *testing.T) {
	defs := []llm.ToolDef{
		{Function: llm.FunctionDef{Name: "read_file"}},
		{Function: llm.FunctionDef{Name: "edit_file"}},
	}
	a := &Agent{
		mode:              event.ModeExecution,
		toolDefs:          defs,
		planningBlocklist: map[string]bool{"edit_file": true},
	}
	if len(a.activeToolDefs()) != len(defs) {
		t.Errorf("execution mode must not filter; got %d", len(a.activeToolDefs()))
	}
}

// --- foundationSteering ---

func TestFoundationSteering_NarrativeFiresWhenOutstandingAndAllComplete(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: ""}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	events := make(chan event.Event, 8)
	a := &Agent{events: events, workspace: ws}

	msgs := []llm.Message{
		{Role: "user", Content: "wrap up"},
		{Role: "assistant", Content: "All done. Outstanding work still needed: refactor lint."},
	}
	narrativeFired, permissionFired := false, false
	out := a.foundationSteering(msgs, &narrativeFired, &permissionFired)
	if len(out) != 1 {
		t.Fatalf("expected one steering message, got %d", len(out))
	}
	if out[0].Role != "user" {
		t.Errorf("expected user role, got %q", out[0].Role)
	}
	if out[0].Content != nudges.OutstandingNudgeMessage {
		t.Errorf("expected OutstandingNudgeMessage, got %q", out[0].Content)
	}
	if !narrativeFired {
		t.Errorf("expected narrativeFired=true after firing")
	}
	if permissionFired {
		t.Errorf("permissionFired must remain false")
	}
}

func TestFoundationSteering_NarrativeOneShot(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: ""}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	events := make(chan event.Event, 8)
	a := &Agent{events: events, workspace: ws}

	msgs := []llm.Message{
		{Role: "assistant", Content: "All set. Items still needed: x."},
	}
	narrativeFired := true // already fired
	permissionFired := false
	if out := a.foundationSteering(msgs, &narrativeFired, &permissionFired); out != nil {
		t.Errorf("expected nil when narrativeFired latched, got %d msgs", len(out))
	}
}

func TestFoundationSteering_PermissionOnlyAutonomous(t *testing.T) {
	events := make(chan event.Event, 8)
	a := &Agent{events: events, autonomous: false}

	msgs := []llm.Message{
		{Role: "assistant", Content: "Should I proceed with the refactor?"},
	}
	narrativeFired, permissionFired := true, false // narrative latched so permission gets checked
	out := a.foundationSteering(msgs, &narrativeFired, &permissionFired)
	if out != nil {
		t.Errorf("permission gate must not fire outside autonomous mode")
	}
}

func TestFoundationSteering_PermissionFiresInAutonomous(t *testing.T) {
	events := make(chan event.Event, 8)
	a := &Agent{events: events, autonomous: true}

	msgs := []llm.Message{
		{Role: "assistant", Content: "Should I proceed with the refactor?"},
	}
	narrativeFired := true // make narrative latch so we test permission path
	permissionFired := false
	out := a.foundationSteering(msgs, &narrativeFired, &permissionFired)
	if len(out) != 1 {
		t.Fatalf("expected permission steering, got %d", len(out))
	}
	if !permissionFired {
		t.Errorf("expected permissionFired=true after firing")
	}
}

func TestFoundationSteering_NoFireWhenNothingMatches(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: "Phase 1 > task"}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	events := make(chan event.Event, 4)
	a := &Agent{events: events, workspace: ws, autonomous: false}

	msgs := []llm.Message{
		{Role: "assistant", Content: "Refactored the helper. Tests green."},
	}
	narrativeFired, permissionFired := false, false
	if out := a.foundationSteering(msgs, &narrativeFired, &permissionFired); out != nil {
		t.Errorf("expected nil when no nudge condition matches; got %d msgs", len(out))
	}
}

// --- isFreshUserInput ---

func TestIsFreshUserInput_TrailingUserResetsBudget(t *testing.T) {
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"empty", nil, false},
		{"trailing tool", []llm.Message{{Role: "tool", Content: "x"}}, false},
		{"trailing assistant", []llm.Message{{Role: "assistant", Content: "x"}}, false},
		{"trailing fresh user", []llm.Message{{Role: "user", Content: "do the thing"}}, true},
		{"trailing narrative nudge", []llm.Message{{Role: "user", Content: nudges.OutstandingNudgeMessage}}, false},
		{"trailing permission nudge", []llm.Message{{Role: "user", Content: nudges.PermissionNudgeMessage}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFreshUserInput(tc.msgs); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// --- FoundationHooks GetSteeringMessages composition ---

func TestFoundationHooks_SteeringResetByFreshInput(t *testing.T) {
	// Lifecycle: assistant produces outstanding-work prose → narrative
	// fires once → next call is no-op (one-shot) → fresh user input
	// arrives via TransformContext → narrative fires again on the
	// next assistant turn matching the same pattern.
	tracker := &gateTracker{loaded: true, activePath: ""}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	events := make(chan event.Event, 16)
	a := &Agent{
		events:          events,
		workspace:       ws,
		mode:            event.ModeExecution,
		interactionMode: Interactive,
		autonomous:      false,
	}

	// Live-message snapshot the steering hook reads. Mutated by the
	// test as the conversation evolves.
	var transcript []llm.Message
	hooks := a.FoundationHooks(func() []llm.Message { return transcript })
	ctx := context.Background()

	// Turn 1: assistant claims done with outstanding markers.
	transcript = []llm.Message{
		{Role: "user", Content: "finish it up"},
		{Role: "assistant", Content: "All set. Outstanding work still needed: tests."},
	}
	out, err := hooks.GetSteeringMessages(ctx)
	if err != nil {
		t.Fatalf("first steering: %v", err)
	}
	if len(out) != 1 || out[0].Content != nudges.OutstandingNudgeMessage {
		t.Fatalf("expected narrative nudge first call, got %+v", out)
	}

	// Append nudge to transcript (the foundation loop does this for us).
	transcript = append(transcript, out...)

	// Second steering call within the same input window: one-shot.
	out, err = hooks.GetSteeringMessages(ctx)
	if err != nil {
		t.Fatalf("second steering: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil on second call (one-shot), got %d msgs", len(out))
	}

	// Fresh user input arrives → TransformContext fires.
	transcript = append(transcript, llm.Message{Role: "user", Content: "ok keep going"})
	if _, err := hooks.TransformContext(ctx, transcript); err != nil {
		t.Fatalf("TransformContext: %v", err)
	}

	// Assistant produces the same outstanding-work response again.
	transcript = append(transcript, llm.Message{Role: "assistant", Content: "Still done. Items still needed: tests."})

	// Steering must fire again (budget reset).
	out, err = hooks.GetSteeringMessages(ctx)
	if err != nil {
		t.Fatalf("third steering: %v", err)
	}
	if len(out) != 1 || out[0].Content != nudges.OutstandingNudgeMessage {
		t.Errorf("expected narrative nudge after fresh input reset; got %+v", out)
	}
}

// --- lintPendingGate ---

func TestLintPendingGate_BlocksWhenPending(t *testing.T) {
	a := &Agent{pendingLint: "vet: declared but not used"}
	res := a.lintPendingGate()
	if !res.Block {
		t.Fatalf("expected Block when pendingLint set")
	}
	if !strings.Contains(res.Reason, "fix style lint violations first") {
		t.Errorf("expected 'fix style lint violations first' message, got: %q", res.Reason)
	}
}

func TestLintPendingGate_PassesWhenEmpty(t *testing.T) {
	a := &Agent{}
	res := a.lintPendingGate()
	if res.Block {
		t.Errorf("expected non-block when pendingLint empty")
	}
}

// --- foundationAfterToolCall ---

func TestFoundationAfterToolCall_AppendsIntentReminderOnSuccess(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &Agent{
		events: events,
		cache:  NewFileCache(),
		intent: "rewrite the auth middleware",
	}
	c := upagent.AfterToolCallContext{
		Name:   "edit_file",
		Result: upagent.ToolResult{Content: "edit applied"},
	}

	got := a.foundationAfterToolCall(c, false)
	if got.Content == nil {
		t.Fatalf("expected Content override; got nil")
	}
	if !strings.Contains(*got.Content, "edit applied") {
		t.Errorf("expected original body to be preserved, got: %q", *got.Content)
	}
	if !strings.Contains(*got.Content, "rewrite the auth middleware") {
		t.Errorf("expected intent reminder appended, got: %q", *got.Content)
	}
	if !strings.Contains(*got.Content, "Reminder — developer's intent") {
		t.Errorf("expected reminder preamble, got: %q", *got.Content)
	}
}

func TestFoundationAfterToolCall_NoIntentNoOverride(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &Agent{
		events: events,
		cache:  NewFileCache(),
		// intent left empty
	}
	c := upagent.AfterToolCallContext{
		Name:   "read_file",
		Result: upagent.ToolResult{Content: "file contents"},
	}
	got := a.foundationAfterToolCall(c, false)
	if got.Content != nil {
		t.Errorf("expected no Content override when intent is empty, got %q", *got.Content)
	}
}

func TestFoundationAfterToolCall_BlockedSkipsReminder(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &Agent{
		events: events,
		cache:  NewFileCache(),
		intent: "do the thing",
	}
	c := upagent.AfterToolCallContext{
		Name:   "edit_file",
		Result: upagent.ToolResult{Content: "Skipped — only ONE file-edit per turn", IsError: true},
	}

	got := a.foundationAfterToolCall(c, true) // blocked=true mirrors BeforeToolCall.Block path
	if got.Content != nil {
		t.Errorf("expected no Content override on blocked path, got: %q", *got.Content)
	}
}

func TestFoundationAfterToolCall_BashFiresReloadBuffers(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &Agent{
		events: events,
		cache:  NewFileCache(),
	}
	c := upagent.AfterToolCallContext{
		Name:   "bash",
		Result: upagent.ToolResult{Content: "ls output"},
	}
	a.foundationAfterToolCall(c, false)

	select {
	case ev := <-events:
		if _, ok := ev.(event.ReloadBuffers); !ok {
			t.Errorf("expected ReloadBuffers event, got %T", ev)
		}
	default:
		t.Errorf("expected ReloadBuffers event to be emitted")
	}
}

func TestFoundationAfterToolCall_BashFiresEvenWhenBlocked(t *testing.T) {
	// Inline behavior: afterToolDispatch fires unconditionally after
	// dispatchTool, including the planning + active-task blocked early
	// returns. Preserve that.
	events := make(chan event.Event, 4)
	a := &Agent{
		events: events,
		cache:  NewFileCache(),
	}
	c := upagent.AfterToolCallContext{
		Name:   "bash",
		Result: upagent.ToolResult{Content: "Error: tool \"bash\" is not available", IsError: true},
	}
	a.foundationAfterToolCall(c, true)

	select {
	case ev := <-events:
		if _, ok := ev.(event.ReloadBuffers); !ok {
			t.Errorf("expected ReloadBuffers even on blocked-bash path, got %T", ev)
		}
	default:
		t.Errorf("expected ReloadBuffers event on blocked-bash path")
	}
}

func TestFoundationAfterToolCall_NonBashSkipsCacheReset(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &Agent{
		events: events,
		cache:  NewFileCache(),
	}
	c := upagent.AfterToolCallContext{
		Name:   "read_file",
		Result: upagent.ToolResult{Content: "x"},
	}
	a.foundationAfterToolCall(c, false)

	select {
	case ev := <-events:
		if _, ok := ev.(event.ReloadBuffers); ok {
			t.Errorf("ReloadBuffers must not fire for non-bash tools")
		}
	default:
		// expected — no event
	}
}

// --- FoundationHooks Before/AfterToolCall integration ---

func TestFoundationHooks_AfterToolCall_TracksBlockedFlag(t *testing.T) {
	// Block path: BeforeToolCall returns Block → AfterToolCall must
	// see blocked=true and skip the reminder. Non-block path: pair
	// rotates to blocked=false and reminder applies.
	tracker := &gateTracker{loaded: true, activePath: ""}
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	a := &Agent{
		events:    mustDrainEvents(t),
		cache:     NewFileCache(),
		workspace: ws,
		mode:      event.ModeExecution,
		intent:    "do the thing",
	}

	hooks := a.FoundationHooks(nil)
	ctx := context.Background()

	// 1. edit_file blocks (no active task) → AfterToolCall must be a no-op.
	br, err := hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("blocked-path BeforeToolCall: %v", err)
	}
	if !br.Block {
		t.Fatalf("expected first call to be blocked")
	}
	ar, err := hooks.AfterToolCall(ctx, upagent.AfterToolCallContext{
		Name:   "edit_file",
		Result: upagent.ToolResult{Content: br.Reason, IsError: true},
	})
	if err != nil {
		t.Fatalf("blocked-path AfterToolCall: %v", err)
	}
	if ar.Content != nil {
		t.Errorf("expected blocked path to skip reminder; got %q", *ar.Content)
	}

	// 2. Activate a task and try a non-edit tool: BeforeToolCall passes,
	// AfterToolCall appends reminder.
	tracker.activePath = "Phase 1 > task"
	br, err = hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "read_file"})
	if err != nil {
		t.Fatalf("non-blocked BeforeToolCall: %v", err)
	}
	if br.Block {
		t.Fatalf("expected read_file to pass; reason=%q", br.Reason)
	}
	ar, err = hooks.AfterToolCall(ctx, upagent.AfterToolCallContext{
		Name:   "read_file",
		Result: upagent.ToolResult{Content: "file body"},
	})
	if err != nil {
		t.Fatalf("non-blocked AfterToolCall: %v", err)
	}
	if ar.Content == nil || !strings.Contains(*ar.Content, "do the thing") {
		t.Errorf("expected reminder appended on non-blocked path, got %v", ar.Content)
	}
}

// --- foundationBudgetCheck ---

func TestFoundationBudgetCheck_NoBudgetIsNoop(t *testing.T) {
	// taskTokenBudget == 0 means "disabled" per budget.Exceeded.
	a := &Agent{
		taskTokenBudget: 0,
		sessionUsage:    budget.Session{TotalPromptTokens: 999_999_999},
	}
	if err := a.foundationBudgetCheck(); err != nil {
		t.Errorf("disabled budget must not abort, got: %v", err)
	}
}

func TestFoundationBudgetCheck_UnderThresholdPasses(t *testing.T) {
	a := &Agent{
		taskTokenBudget: 1_000,
		sessionUsage:    budget.Session{TotalPromptTokens: 100, TotalCompletionTokens: 200},
	}
	if err := a.foundationBudgetCheck(); err != nil {
		t.Errorf("under-threshold must not abort, got: %v", err)
	}
}

func TestFoundationBudgetCheck_OverThresholdAbortsAndLatches(t *testing.T) {
	// foundationBudgetCheck used to emit AgentError directly. After
	// the 8c cutover the user-facing emission is owned by the
	// foundation→engine event translator (Error → AgentError); this
	// hook's contract is now "return a wrapped errBudgetExceeded with
	// the formatted budget message; latch budgetExceeded so subsequent
	// calls return without re-warning."
	a := &Agent{
		taskTokenBudget: 1_000,
		sessionUsage:    budget.Session{TotalPromptTokens: 600, TotalCompletionTokens: 600, Turns: 3},
	}

	err := a.foundationBudgetCheck()
	if !errors.Is(err, errBudgetExceeded) {
		t.Fatalf("expected errBudgetExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected wrapped error to carry the budget message, got %q", err.Error())
	}
	if !a.budgetExceeded {
		t.Errorf("expected budgetExceeded=true after abort")
	}
}

func TestFoundationBudgetCheck_LatchedDoesNotReEmit(t *testing.T) {
	// Already-latched: foundationBudgetCheck must return errBudgetExceeded
	// without re-running the math. Its err.Error() carries only the
	// sentinel text (no formatted budget message) because the latch
	// short-circuits before the wrap.
	a := &Agent{
		taskTokenBudget: 1_000,
		budgetExceeded:  true,
		sessionUsage:    budget.Session{TotalPromptTokens: 999_999_999},
	}

	err := a.foundationBudgetCheck()
	if !errors.Is(err, errBudgetExceeded) {
		t.Fatalf("expected errBudgetExceeded, got %v", err)
	}
	if strings.Contains(err.Error(), "tokens used") {
		t.Errorf("latched return should NOT re-format the budget message; got %q", err.Error())
	}
}

// --- FoundationHooks TransformContext budget integration ---

func TestFoundationHooks_TransformContext_BudgetAbort(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &Agent{
		events:          events,
		taskTokenBudget: 100,
		sessionUsage:    budget.Session{TotalPromptTokens: 80, TotalCompletionTokens: 80},
	}
	hooks := a.FoundationHooks(nil)

	out, err := hooks.TransformContext(context.Background(), nil)
	if !errors.Is(err, errBudgetExceeded) {
		t.Fatalf("expected errBudgetExceeded from TransformContext, got %v", err)
	}
	if out != nil {
		t.Errorf("expected nil msgs on abort, got %d", len(out))
	}
}

// --- TransformContext composed with concern #2 reset ---

func TestFoundationHooks_TransformContext_ResetAndCompactAndLint(t *testing.T) {
	// Mirrors real foundation runtime order: TransformContext fires at
	// turn start (drains pendingLint into messages), THEN BeforeToolCall
	// fires per tool. lintPendingGate would short-circuit any
	// BeforeToolCall that ran with pendingLint still set.
	a := &Agent{
		events:          mustDrainEvents(t),
		cache:           NewFileCache(),
		mode:            event.ModeExecution,
		interactionMode: Interactive,
		autonomous:      false,
		pendingLint:     "violation",
	}
	hooks := a.FoundationHooks(nil)
	ctx := context.Background()

	// Turn 1 start: TransformContext drains pendingLint.
	out, err := hooks.TransformContext(ctx, []llm.Message{{Role: "user", Content: "."}})
	if err != nil {
		t.Fatalf("TransformContext: %v", err)
	}
	if len(out) != 2 || !strings.Contains(out[1].Content, "STOP") {
		t.Errorf("expected lint message appended; got %+v", out)
	}

	// Turn 1 dispatch: pendingLint drained, first edit passes, second blocked.
	r, err := hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("turn 1 first BeforeToolCall: %v", err)
	}
	if r.Block {
		t.Fatalf("first edit should pass after lint drained, got Block=%q", r.Reason)
	}
	r, err = hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("turn 1 second BeforeToolCall: %v", err)
	}
	if !r.Block {
		t.Fatalf("second edit should be blocked")
	}

	// Turn 2 start: TransformContext resets editFired (no lint this time).
	if _, err := hooks.TransformContext(ctx, []llm.Message{{Role: "user", Content: "next"}}); err != nil {
		t.Fatalf("TransformContext turn 2: %v", err)
	}

	// Turn 2 dispatch: first edit must pass again (single-edit reset).
	r, err = hooks.BeforeToolCall(ctx, upagent.BeforeToolCallContext{Name: "edit_file"})
	if err != nil {
		t.Fatalf("turn 2 BeforeToolCall: %v", err)
	}
	if r.Block {
		t.Errorf("post-reset first edit should pass; reason=%q", r.Reason)
	}
}
