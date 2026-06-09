package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// lifecycleTracker is a TaskTracker stub recording activate/complete
// calls and injecting failures, used by the bundle dispatch tests.
// Composed around [gateTracker] so the activePath / loaded knobs from
// the existing gate tests stay available.
type lifecycleTracker struct {
	activePath  string
	loaded      bool
	nextPending string

	activateErr error // when non-nil, ActivateTask returns this
	completeErr error // when non-nil, CompleteTask returns this
	nextActErr  error // when non-nil, the auto-activate-next ActivateTask returns this

	activated []string
	completed []string
}

func (t *lifecycleTracker) ActivateTask(title string) error {
	if t.activateErr != nil {
		return t.activateErr
	}
	// The auto-activate-next call goes through the same method; route
	// its error via nextActErr to distinguish from the first-activate
	// error in tests.
	if t.nextPending != "" && title == t.nextPending && t.nextActErr != nil {
		return t.nextActErr
	}
	t.activated = append(t.activated, title)
	t.activePath = title
	return nil
}

func (t *lifecycleTracker) CompleteTask(title string) error {
	if t.completeErr != nil {
		return t.completeErr
	}
	t.completed = append(t.completed, title)
	t.activePath = ""
	return nil
}

func (t *lifecycleTracker) AddTask(_, _, _, _ string) error    { return nil }
func (t *lifecycleTracker) ActiveTaskPath() string             { return t.activePath }
func (t *lifecycleTracker) WorkTreeLoaded() bool               { return t.loaded }
func (t *lifecycleTracker) NextPendingTask() string            { return t.nextPending }
func (t *lifecycleTracker) InitProject(string, []string) error { return nil }

// lifecycleTestWorkspace satisfies Workspace and TaskTracker.
type lifecycleTestWorkspace struct {
	*testWorkspace
	*lifecycleTracker
}

func newLifecycleAgent(t *testing.T, tracker *lifecycleTracker) *Agent {
	t.Helper()
	ws := &lifecycleTestWorkspace{
		testWorkspace:    &testWorkspace{},
		lifecycleTracker: tracker,
	}
	a := &Agent{
		bus:       newBus(),
		workspace: ws,
		mode:      event.ModeExecution,
	}
	_ = subscribeForTest(t, a)
	return a
}

// --- parseLifecycleBundle ---

func TestParseLifecycleBundle_EmptyArgs(t *testing.T) {
	b := parseLifecycleBundle("")
	if b.ActivateTask != "" || b.CompleteTask {
		t.Errorf("expected zero-value bundle for empty args, got %+v", b)
	}
}

func TestParseLifecycleBundle_NoFields(t *testing.T) {
	b := parseLifecycleBundle(`{"path":"x","content":"y"}`)
	if b.ActivateTask != "" || b.CompleteTask {
		t.Errorf("expected zero-value bundle when fields absent, got %+v", b)
	}
}

func TestParseLifecycleBundle_ActivateOnly(t *testing.T) {
	b := parseLifecycleBundle(`{"path":"x","activate_task":"Implement death sound"}`)
	if b.ActivateTask != "Implement death sound" {
		t.Errorf("ActivateTask = %q", b.ActivateTask)
	}
	if b.CompleteTask {
		t.Errorf("CompleteTask = true, want false")
	}
}

func TestParseLifecycleBundle_CompleteOnly(t *testing.T) {
	b := parseLifecycleBundle(`{"path":"x","complete_task":true}`)
	if b.ActivateTask != "" {
		t.Errorf("ActivateTask = %q", b.ActivateTask)
	}
	if !b.CompleteTask {
		t.Errorf("CompleteTask = false, want true")
	}
}

func TestParseLifecycleBundle_Both(t *testing.T) {
	b := parseLifecycleBundle(`{"activate_task":"X","complete_task":true}`)
	if b.ActivateTask != "X" || !b.CompleteTask {
		t.Errorf("bundle = %+v", b)
	}
}

func TestParseLifecycleBundle_MalformedArgs(t *testing.T) {
	// Tolerated silently — callers treat zero-value as no-op.
	b := parseLifecycleBundle(`{not valid json`)
	if b.ActivateTask != "" || b.CompleteTask {
		t.Errorf("expected zero-value on malformed args, got %+v", b)
	}
}

// --- activeTaskTitle ---

func TestActiveTaskTitle(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"Solo Task", "Solo Task"},
		{"Phase 1 > Setup > Implement X", "Implement X"},
		{"A > B", "B"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := activeTaskTitle(tc.in); got != tc.want {
				t.Errorf("activeTaskTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- runLifecycleActivate ---

func TestRunLifecycleActivate_EmptyIsNoop(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true}
	a := newLifecycleAgent(t, tracker)
	res := a.runLifecycleActivate(context.Background(), lifecycleBundle{}, "edit_file")
	if res.Block {
		t.Errorf("empty bundle must not block; reason=%q", res.Reason)
	}
	if len(tracker.activated) != 0 {
		t.Errorf("empty bundle must not call ActivateTask; got %v", tracker.activated)
	}
}

func TestRunLifecycleActivate_Success(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true}
	a := newLifecycleAgent(t, tracker)
	res := a.runLifecycleActivate(context.Background(), lifecycleBundle{ActivateTask: "Implement X"}, "edit_file")
	if res.Block {
		t.Fatalf("expected success, got block: %q", res.Reason)
	}
	if got := tracker.activated; len(got) != 1 || got[0] != "Implement X" {
		t.Errorf("ActivateTask calls = %v, want [\"Implement X\"]", got)
	}
}

func TestRunLifecycleActivate_FailureBlocks(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activateErr: errors.New("task not found: X")}
	a := newLifecycleAgent(t, tracker)
	res := a.runLifecycleActivate(context.Background(), lifecycleBundle{ActivateTask: "X"}, "edit_file")
	if !res.Block {
		t.Fatalf("expected Block on activate failure")
	}
	if !strings.Contains(res.Reason, "activate_task") || !strings.Contains(res.Reason, "task not found") {
		t.Errorf("reason should name the failure: %q", res.Reason)
	}
}

func TestRunLifecycleActivate_NoTaskTrackerSilent(t *testing.T) {
	// Workspace that doesn't implement TaskTracker — bundle is a no-op,
	// mirroring [enforceActiveTaskGate]'s nil-tracker carve-out.
	a := &Agent{
		bus:       newBus(),
		workspace: &testWorkspace{},
	}
	_ = subscribeForTest(t, a)
	res := a.runLifecycleActivate(context.Background(), lifecycleBundle{ActivateTask: "X"}, "edit_file")
	if res.Block {
		t.Errorf("expected silent no-op without TaskTracker; reason=%q", res.Reason)
	}
}

// --- runLifecycleComplete ---

func TestRunLifecycleComplete_NotRequestedIsNil(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X"}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{},
		upagent.ToolResult{Content: "ok"})
	if got != nil {
		t.Errorf("expected nil override when complete_task unset; got %q", *got)
	}
	if len(tracker.completed) != 0 {
		t.Errorf("CompleteTask must not fire; got %v", tracker.completed)
	}
}

func TestRunLifecycleComplete_ToolErrorSkips(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X"}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{CompleteTask: true},
		upagent.ToolResult{Content: "Error: edit failed", IsError: true})
	if got != nil {
		t.Errorf("tool error must not trigger complete; got %q", *got)
	}
	if len(tracker.completed) != 0 {
		t.Errorf("CompleteTask must not fire; got %v", tracker.completed)
	}
}

func TestRunLifecycleComplete_SuccessNoNextTask(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X"}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{CompleteTask: true},
		upagent.ToolResult{Content: "edit applied"})
	if got == nil {
		t.Fatalf("expected non-nil override on success")
	}
	if !strings.HasPrefix(*got, "edit applied") {
		t.Errorf("override must preserve original content; got %q", *got)
	}
	if !strings.Contains(*got, "Task completed: X") {
		t.Errorf("override missing completion line; got %q", *got)
	}
	if !strings.Contains(*got, "All tasks complete") {
		t.Errorf("override missing terminal hint; got %q", *got)
	}
	if got := tracker.completed; len(got) != 1 || got[0] != "X" {
		t.Errorf("CompleteTask = %v, want [X]", got)
	}
}

func TestRunLifecycleComplete_SuccessAutoActivatesNext(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X", nextPending: "Y"}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{CompleteTask: true},
		upagent.ToolResult{Content: "edit applied"})
	if got == nil {
		t.Fatalf("expected non-nil override")
	}
	if !strings.Contains(*got, "Next task auto-activated: Y") {
		t.Errorf("override missing next-task line; got %q", *got)
	}
	if len(tracker.activated) != 1 || tracker.activated[0] != "Y" {
		t.Errorf("expected Y auto-activated; activated = %v", tracker.activated)
	}
}

func TestRunLifecycleComplete_CompleteFailurePreservesResult(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X", completeErr: errors.New("persist failed")}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{CompleteTask: true},
		upagent.ToolResult{Content: "edit applied"})
	if got == nil {
		t.Fatalf("expected non-nil override even on complete failure")
	}
	if !strings.HasPrefix(*got, "edit applied") {
		t.Errorf("must preserve original content; got %q", *got)
	}
	if !strings.Contains(*got, "auto-complete failed") || !strings.Contains(*got, "persist failed") {
		t.Errorf("must surface the complete error; got %q", *got)
	}
}

func TestRunLifecycleComplete_NextActivateFailureSurfaces(t *testing.T) {
	tracker := &lifecycleTracker{
		loaded:      true,
		activePath:  "X",
		nextPending: "Y",
		nextActErr:  errors.New("conflict"),
	}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{CompleteTask: true},
		upagent.ToolResult{Content: "edit applied"})
	if got == nil {
		t.Fatalf("expected non-nil override")
	}
	if !strings.Contains(*got, "Task completed: X") {
		t.Errorf("missing complete line; got %q", *got)
	}
	if !strings.Contains(*got, "Next pending task:") || !strings.Contains(*got, "conflict") {
		t.Errorf("missing next-activate failure line; got %q", *got)
	}
}

func TestRunLifecycleComplete_NoActiveTaskWarning(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: ""}
	a := newLifecycleAgent(t, tracker)
	got := a.runLifecycleComplete(context.Background(),
		lifecycleBundle{CompleteTask: true},
		upagent.ToolResult{Content: "edit applied"})
	if got == nil {
		t.Fatalf("expected non-nil override warning")
	}
	if !strings.Contains(*got, "no active task") {
		t.Errorf("missing warning; got %q", *got)
	}
}

// --- FoundationHooks integration: composed dispatch ---

func TestFoundationHooks_BeforeToolCall_EmptyBundleNoChange(t *testing.T) {
	// Strict back-compat: with no lifecycle fields, behavior matches the
	// pre-bundle dispatch — gate blocks because no task is active.
	tracker := &lifecycleTracker{loaded: true, activePath: ""}
	a := newLifecycleAgent(t, tracker)
	hooks := a.FoundationHooks(nil)

	res, err := hooks.BeforeToolCall(context.Background(), upagent.BeforeToolCallInput{
		Name: "edit_file",
		Args: `{"path":"main.go"}`,
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if !res.Block || !strings.Contains(res.Reason, "no active task") {
		t.Errorf("expected gate to block (no bundle, no active task); got %+v", res)
	}
	if len(tracker.activated) != 0 {
		t.Errorf("ActivateTask must not fire for empty bundle; got %v", tracker.activated)
	}
}

func TestFoundationHooks_BeforeToolCall_ActivateUnlocksGate(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: ""}
	a := newLifecycleAgent(t, tracker)
	hooks := a.FoundationHooks(nil)

	res, err := hooks.BeforeToolCall(context.Background(), upagent.BeforeToolCallInput{
		Name: "edit_file",
		Args: `{"path":"main.go","activate_task":"Implement X"}`,
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if res.Block {
		t.Errorf("expected dispatch to proceed after bundle activate; reason=%q", res.Reason)
	}
	if got := tracker.activated; len(got) != 1 || got[0] != "Implement X" {
		t.Errorf("ActivateTask calls = %v, want [Implement X]", got)
	}
}

func TestFoundationHooks_BeforeToolCall_ActivateFailureAborts(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activateErr: errors.New("task not found: Z")}
	a := newLifecycleAgent(t, tracker)
	hooks := a.FoundationHooks(nil)

	res, err := hooks.BeforeToolCall(context.Background(), upagent.BeforeToolCallInput{
		Name: "edit_file",
		Args: `{"path":"main.go","activate_task":"Z"}`,
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if !res.Block {
		t.Fatalf("expected Block on activate failure")
	}
	if !strings.Contains(res.Reason, "activate_task") {
		t.Errorf("block reason must reference activate_task; got %q", res.Reason)
	}
}

func TestFoundationHooks_BeforeToolCall_NonMutatingSkipsBundle(t *testing.T) {
	// Non-mutating tools never touch the bundle even if the LLM somehow
	// includes the fields. The gate also lets them through unconditionally.
	tracker := &lifecycleTracker{loaded: true, activePath: ""}
	a := newLifecycleAgent(t, tracker)
	hooks := a.FoundationHooks(nil)

	res, err := hooks.BeforeToolCall(context.Background(), upagent.BeforeToolCallInput{
		Name: "read_file",
		Args: `{"path":"main.go","activate_task":"Should be ignored"}`,
	})
	if err != nil {
		t.Fatalf("BeforeToolCall: %v", err)
	}
	if res.Block {
		t.Errorf("non-mutating tool should not be blocked; got %+v", res)
	}
	if len(tracker.activated) != 0 {
		t.Errorf("non-mutating tool must not trigger activate; got %v", tracker.activated)
	}
}

func TestFoundationHooks_AfterToolCall_CompleteAppendsTrailer(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X"}
	a := newLifecycleAgent(t, tracker)
	a.cache = NewFileCache()
	hooks := a.FoundationHooks(nil)

	res, err := hooks.AfterToolCall(context.Background(), upagent.AfterToolCallInput{
		Name:   "edit_file",
		Args:   `{"path":"main.go","complete_task":true}`,
		Result: upagent.ToolResult{Content: "edit applied"},
	})
	if err != nil {
		t.Fatalf("AfterToolCall: %v", err)
	}
	if res.Content == nil {
		t.Fatalf("expected Content override after successful complete")
	}
	if !strings.Contains(*res.Content, "Task completed: X") {
		t.Errorf("override missing complete line; got %q", *res.Content)
	}
	if len(tracker.completed) != 1 || tracker.completed[0] != "X" {
		t.Errorf("CompleteTask = %v, want [X]", tracker.completed)
	}
}

func TestFoundationHooks_AfterToolCall_ToolErrorSkipsComplete(t *testing.T) {
	tracker := &lifecycleTracker{loaded: true, activePath: "X"}
	a := newLifecycleAgent(t, tracker)
	a.cache = NewFileCache()
	hooks := a.FoundationHooks(nil)

	res, err := hooks.AfterToolCall(context.Background(), upagent.AfterToolCallInput{
		Name:   "edit_file",
		Args:   `{"path":"main.go","complete_task":true}`,
		Result: upagent.ToolResult{Content: "Error: edit failed", IsError: true},
	})
	if err != nil {
		t.Fatalf("AfterToolCall: %v", err)
	}
	if res.Content != nil {
		t.Errorf("tool error must not trigger complete or override; got %q", *res.Content)
	}
	if len(tracker.completed) != 0 {
		t.Errorf("CompleteTask must not fire on tool error; got %v", tracker.completed)
	}
}

// --- Schema augmentation (Commit 2) ---

// fakeTool is a minimal Tool implementation for schema-augmentation
// tests. Execute is unused — the tests only exercise Definition().
type fakeTool struct {
	def llm.ToolDef
}

func (f fakeTool) Definition() llm.ToolDef { return f.def }
func (f fakeTool) Execute(_ context.Context, _ llm.ToolCall) upagent.ToolResult {
	return upagent.ToolResult{}
}

func sampleDef(name string) llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        name,
			Description: name + " desc",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {Type: "string", Description: "Path"},
				},
				Required: []string{"path"},
			},
		},
	}
}

func TestAugmentWithLifecycleFields_AddsBothFields(t *testing.T) {
	out := augmentWithLifecycleFields(sampleDef("edit_file"))
	props := out.Function.Parameters.Properties
	activate, ok := props["activate_task"]
	if !ok {
		t.Fatalf("activate_task missing from properties: %+v", props)
	}
	if activate.Type != "string" {
		t.Errorf("activate_task type = %q, want \"string\"", activate.Type)
	}
	complete, ok := props["complete_task"]
	if !ok {
		t.Fatalf("complete_task missing from properties: %+v", props)
	}
	if complete.Type != "boolean" {
		t.Errorf("complete_task type = %q, want \"boolean\"", complete.Type)
	}
}

func TestAugmentWithLifecycleFields_PreservesExistingProperties(t *testing.T) {
	out := augmentWithLifecycleFields(sampleDef("edit_file"))
	if _, ok := out.Function.Parameters.Properties["path"]; !ok {
		t.Errorf("existing 'path' property dropped after augmentation")
	}
}

func TestAugmentWithLifecycleFields_DoesNotChangeRequired(t *testing.T) {
	// Lifecycle fields are strictly additive — required-list must NOT
	// gain them so existing call shapes keep validating.
	out := augmentWithLifecycleFields(sampleDef("edit_file"))
	for _, name := range out.Function.Parameters.Required {
		if name == "activate_task" || name == "complete_task" {
			t.Errorf("lifecycle field %q must not appear in Required", name)
		}
	}
}

func TestAugmentWithLifecycleFields_DoesNotMutateInput(t *testing.T) {
	// The augmenter must copy Properties — mutating the input would
	// poison the underlying tool's cached schema (tests reuse fixtures).
	in := sampleDef("edit_file")
	_ = augmentWithLifecycleFields(in)
	if _, leaked := in.Function.Parameters.Properties["activate_task"]; leaked {
		t.Errorf("augmentWithLifecycleFields mutated input Properties")
	}
}

func TestAugmentWithLifecycleFields_Idempotent(t *testing.T) {
	once := augmentWithLifecycleFields(sampleDef("edit_file"))
	twice := augmentWithLifecycleFields(once)
	// Idempotent: a second pass returns the same shape (same field
	// count, same descriptions).
	if len(once.Function.Parameters.Properties) != len(twice.Function.Parameters.Properties) {
		t.Errorf("idempotency: prop counts differ: %d vs %d",
			len(once.Function.Parameters.Properties),
			len(twice.Function.Parameters.Properties))
	}
}

func TestLifecycleAwareTool_DefinitionAdvertisesFields(t *testing.T) {
	wrapped := lifecycleAwareTool{Tool: fakeTool{def: sampleDef("edit_file")}}
	def := wrapped.Definition()
	if _, ok := def.Function.Parameters.Properties["activate_task"]; !ok {
		t.Errorf("wrapped Definition missing activate_task")
	}
	if _, ok := def.Function.Parameters.Properties["complete_task"]; !ok {
		t.Errorf("wrapped Definition missing complete_task")
	}
}

func TestApplyLifecycleDecorator_MutatingToolWrapped(t *testing.T) {
	base := fakeTool{def: sampleDef("edit_file")}
	got, def := applyLifecycleDecorator("edit_file", base, base.Definition())
	if _, ok := got.(lifecycleAwareTool); !ok {
		t.Errorf("expected mutating tool to be wrapped in lifecycleAwareTool; got %T", got)
	}
	if _, ok := def.Function.Parameters.Properties["activate_task"]; !ok {
		t.Errorf("returned def must already carry activate_task")
	}
}

func TestApplyLifecycleDecorator_NonMutatingToolUnwrapped(t *testing.T) {
	base := fakeTool{def: sampleDef("read_file")}
	got, def := applyLifecycleDecorator("read_file", base, base.Definition())
	if _, ok := got.(lifecycleAwareTool); ok {
		t.Errorf("non-mutating tool must NOT be wrapped")
	}
	if _, ok := def.Function.Parameters.Properties["activate_task"]; ok {
		t.Errorf("non-mutating tool def must NOT carry activate_task; got %+v",
			def.Function.Parameters.Properties)
	}
}

func TestMutatingTools_IncludesApplyPatch(t *testing.T) {
	// Gap fix: apply_patch landed in PR #173 as a fourth file-mutation
	// primitive but was never registered as mutating, so it skipped the
	// active-task gate AND missed the lifecycle schema. Lock that
	// regression here.
	if !mutatingTools["apply_patch"] {
		t.Errorf("apply_patch must be in mutatingTools for the active-task gate + lifecycle schema to fire")
	}
}

func TestMutatingTools_KnownMembers(t *testing.T) {
	want := []string{"edit_file", "write_file", "replace_file", "apply_patch", "bash", "smoke_run"}
	for _, name := range want {
		if !mutatingTools[name] {
			t.Errorf("expected %q in mutatingTools", name)
		}
	}
}

func TestFoundationHooks_AfterToolCall_EmptyBundlePassesThrough(t *testing.T) {
	// Strict back-compat: no bundle fields → res.Content remains nil
	// (the original tool result content is forwarded to the LLM
	// unchanged by the foundation loop).
	tracker := &lifecycleTracker{loaded: true, activePath: "X"}
	a := newLifecycleAgent(t, tracker)
	a.cache = NewFileCache()
	hooks := a.FoundationHooks(nil)

	res, err := hooks.AfterToolCall(context.Background(), upagent.AfterToolCallInput{
		Name:   "edit_file",
		Args:   `{"path":"main.go"}`,
		Result: upagent.ToolResult{Content: "edit applied"},
	})
	if err != nil {
		t.Fatalf("AfterToolCall: %v", err)
	}
	if res.Content != nil {
		t.Errorf("empty bundle must leave Content nil; got override %q", *res.Content)
	}
	if len(tracker.completed) != 0 {
		t.Errorf("CompleteTask must not fire; got %v", tracker.completed)
	}
}
