package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTracker captures AddTask calls for assertion. addErr applies
// uniformly to every call; addErrFn overrides it per-call when a test
// needs to script different outcomes (e.g., first call fails, second
// succeeds — to exercise the post-AddTask dedup-register ordering).
type stubTracker struct {
	addCalls []struct{ phase, feature, task, link string }
	addErr   error
	addErrFn func(p, f, t string) error
}

func (s *stubTracker) ActivateTask(string) error          { return nil }
func (s *stubTracker) CompleteTask(string) error          { return nil }
func (s *stubTracker) ActiveTaskPath() string             { return "" }
func (s *stubTracker) WorkTreeLoaded() bool               { return true }
func (s *stubTracker) NextPendingTask() string            { return "" }
func (s *stubTracker) InitProject(string, []string) error { return nil }
func (s *stubTracker) AddTask(p, f, t, l string) error {
	s.addCalls = append(s.addCalls, struct{ phase, feature, task, link string }{p, f, t, l})
	if s.addErrFn != nil {
		return s.addErrFn(p, f, t)
	}
	return s.addErr
}

func TestProjectTaskAddTool_SingleTask(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"tasks": [{"phase": "Phase 1", "feature": "Render", "task": "draw sprites", "link": "/game/sprites.md"}]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))

	assertContains(t, result.Content, "Added 1 task(s)")
	assertContains(t, result.Content, "Phase 1 > Render > draw sprites")
	if len(tracker.addCalls) != 1 {
		t.Fatalf("expected 1 AddTask call, got %d", len(tracker.addCalls))
	}
	call := tracker.addCalls[0]
	if call.phase != "Phase 1" || call.feature != "Render" ||
		call.task != "draw sprites" || call.link != "/game/sprites.md" {
		t.Errorf("wrong args passed to tracker: %+v", call)
	}
}

// TestProjectTaskAddTool_Batch is the core win of the batch shape: one
// tool call dispatches AddTask across multiple distinct phase/feature
// pairs in order. The Pac-Man trace consistently saw the LLM
// back-to-back the per-task calls in a single turn; with this shape
// the same intent collapses to one tool call and one tool result.
func TestProjectTaskAddTool_Batch(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"tasks": [
		{"phase": "Foundation", "feature": "Project Setup", "task": "Create LÖVE bootstrap"},
		{"phase": "Foundation", "feature": "Project Setup", "task": "Implement tile maze"},
		{"phase": "Ghosts and AI", "feature": "Ghost Behavior", "task": "Implement chase/scatter"},
		{"phase": "Audio and Polish", "feature": "Presentation", "task": "Synthesized SFX"}
	]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))

	assertContains(t, result.Content, "Added 4 task(s)")
	if len(tracker.addCalls) != 4 {
		t.Fatalf("expected 4 AddTask calls, got %d", len(tracker.addCalls))
	}
	// Order preservation matters: the LLM may rely on task order for
	// the activation gate (e.g., NextPendingTask returning the first
	// pending in document order — which is the order we appended).
	wantTitles := []string{
		"Create LÖVE bootstrap",
		"Implement tile maze",
		"Implement chase/scatter",
		"Synthesized SFX",
	}
	for i, want := range wantTitles {
		if tracker.addCalls[i].task != want {
			t.Errorf("call %d: task = %q, want %q", i, tracker.addCalls[i].task, want)
		}
	}
}

// TestProjectTaskAddTool_PartialSuccess locks the partial-success
// contract: a malformed entry in the middle of the batch must not
// stop the surrounding entries from being added. The result names
// what succeeded AND what failed so the LLM can retry only the
// failures instead of resending the whole batch.
func TestProjectTaskAddTool_PartialSuccess(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"tasks": [
		{"phase": "Foundation", "feature": "Setup", "task": "good one"},
		{"phase": "", "feature": "Setup", "task": "bad — missing phase"},
		{"phase": "Foundation", "feature": "Setup", "task": "another good one"}
	]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))

	assertContains(t, result.Content, "Added 2 task(s)")
	assertContains(t, result.Content, "good one")
	assertContains(t, result.Content, "another good one")
	assertContains(t, result.Content, "Failed 1 task(s)")
	assertContains(t, result.Content, "tasks[1]")
	assertContains(t, result.Content, "phase is required")
	if len(tracker.addCalls) != 2 {
		t.Fatalf("expected 2 AddTask calls (1 skipped), got %d", len(tracker.addCalls))
	}
}

// TestProjectTaskAddTool_DedupesWithinBatch locks the within-batch
// dedup guard. The LLM occasionally repeats a task triple inside one
// batch (e.g., a planning prompt that sketches the same item twice).
// Without the guard, both entries would pass validation, fire
// AddTask twice, and write two identical `[ ]` bullets to
// /project.md — the second copy would then orphan as soon as
// update_task / NextPendingTask matched the first by title.
//
// Surfacing the duplicate as a "failed" entry (rather than silently
// skipping) lets the LLM see that one of its entries was a mistake.
// The first occurrence still succeeds; only the repeats fail.
//
// Normalization that runs BEFORE the dedup check:
//   - whitespace: "Foundation" and "  Foundation\n" → same key
//   - case: "Foundation" and "foundation" → same key (matches the
//     tracker's case-insensitive phase + feature resolution; task is
//     folded too so an LLM case slip doesn't write a sibling bullet)
//
// Only phase + feature + task participate. `link` is supplementary
// context and not part of task identity, so entries that differ only
// in link still count as duplicates.
func TestProjectTaskAddTool_DedupesWithinBatch(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"tasks":[
		{"phase":"Foundation","feature":"Setup","task":"Implement maze"},
		{"phase":"Foundation","feature":"Setup","task":"Implement ghosts"},
		{"phase":"  Foundation  ","feature":"Setup","task":"Implement maze"},
		{"phase":"Foundation","feature":"Setup","task":"Implement maze","link":"/different.md"},
		{"phase":"foundation","feature":"setup","task":"IMPLEMENT MAZE"}
	]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))

	// tasks[0] and tasks[1] are unique — both should be added.
	// tasks[2] is the same triple as tasks[0] after whitespace
	// normalization — should fail as duplicate of tasks[0].
	// tasks[3] differs only in link, which doesn't participate in
	// the dedup key — should also fail as duplicate of tasks[0].
	// tasks[4] differs only in case across all three fields — should
	// ALSO fail as duplicate of tasks[0] because the tracker resolves
	// phase/feature case-insensitively and we fold task too to defend
	// against LLM case slips that would otherwise write a sibling
	// bullet under the same feature.
	assertContains(t, result.Content, "Added 2 task(s)")
	assertContains(t, result.Content, "Implement maze")
	assertContains(t, result.Content, "Implement ghosts")
	assertContains(t, result.Content, "Failed 3 task(s)")
	assertContains(t, result.Content, "tasks[2]")
	assertContains(t, result.Content, "duplicate of tasks[0]")
	assertContains(t, result.Content, "tasks[3]")
	assertContains(t, result.Content, "tasks[4]")
	if len(tracker.addCalls) != 2 {
		t.Fatalf("expected 2 AddTask calls (3 dedup'd), got %d", len(tracker.addCalls))
	}
}

// TestProjectTaskAddTool_FailedFirstOccurrenceDoesNotBlockRetry locks
// the post-AddTask dedup-register ordering. Earlier draft registered
// the dedup key BEFORE the AddTask call, so a tracker failure on
// tasks[i] would silently block any later identical triple — even
// though tasks[i] never wrote a bullet to /project.md. The documented
// partial-success contract says tracker errors don't stop the others,
// which must include later attempts at the same triple.
//
// Setup: tasks[0] and tasks[2] are the same triple. The tracker is
// scripted to fail on the first AddTask but succeed on the second.
// Expected: AddTask is called twice (NOT blocked as "duplicate"), and
// tasks[2] lands in `added`, not `failed`. tasks[1] is a sanity-check
// distinct entry that should also succeed.
func TestProjectTaskAddTool_FailedFirstOccurrenceDoesNotBlockRetry(t *testing.T) {
	calls := 0
	tracker := &stubTracker{
		addErrFn: func(p, f, t string) error {
			calls++
			if calls == 1 && p == "Foundation" && t == "Implement maze" {
				return errors.New("transient: tree not yet loaded")
			}
			return nil
		},
	}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"tasks":[
		{"phase":"Foundation","feature":"Setup","task":"Implement maze"},
		{"phase":"Foundation","feature":"Setup","task":"Implement ghosts"},
		{"phase":"Foundation","feature":"Setup","task":"Implement maze"}
	]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))

	// tasks[0] fails (tracker error), tasks[1] succeeds, tasks[2]
	// retries the same triple as tasks[0] and succeeds — must NOT be
	// rejected as a duplicate, since tasks[0] never wrote a bullet.
	assertContains(t, result.Content, "Added 2 task(s)")
	assertContains(t, result.Content, "Implement ghosts")
	assertContains(t, result.Content, "Failed 1 task(s)")
	assertContains(t, result.Content, "tasks[0]")
	assertContains(t, result.Content, "transient: tree not yet loaded")
	if strings.Contains(result.Content, "duplicate of tasks[0]") {
		t.Errorf("tasks[2] must NOT be rejected as duplicate when tasks[0] failed; got:\n%s", result.Content)
	}
	// Three AddTask calls: tasks[0] (fails), tasks[1] (succeeds),
	// tasks[2] (succeeds — the retry). Without the fix this would be 2,
	// because tasks[2] would be silently blocked as a duplicate.
	if len(tracker.addCalls) != 3 {
		t.Fatalf("expected 3 AddTask calls (incl. retry), got %d", len(tracker.addCalls))
	}
}

func TestProjectTaskAddTool_Validation(t *testing.T) {
	tool := NewProjectTaskAddTool(&stubTracker{})
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing phase in entry", `{"tasks":[{"feature":"F","task":"t"}]}`, "phase is required"},
		{"missing feature in entry", `{"tasks":[{"phase":"P","task":"t"}]}`, "feature is required"},
		{"missing task in entry", `{"tasks":[{"phase":"P","feature":"F"}]}`, "task is required"},
		{"empty tasks array", `{"tasks":[]}`, "tasks array is required and must not be empty"},
		{"missing tasks field", `{}`, "tasks array is required and must not be empty"},
		{"bad json", `{not json`, "invalid arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", tc.args))
			assertContains(t, result.Content, tc.want)
		})
	}
}

func TestProjectTaskAddTool_NilTracker(t *testing.T) {
	tool := NewProjectTaskAddTool(nil)
	args := `{"tasks":[{"phase":"P","feature":"F","task":"t"}]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))
	assertContains(t, result.Content, "task tracking not available")
}

func TestProjectTaskAddTool_NormalizesWhitespace(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	// Whitespace-padded fields must reach the tracker trimmed so lookups
	// against "Phase 1" / "Render" succeed.
	args := `{"tasks":[{"phase":"  Phase 1  ","feature":"\tRender\n","task":"  draw  ","link":"  /x.md  "}]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))
	assertContains(t, result.Content, "Phase 1 > Render > draw")

	if len(tracker.addCalls) != 1 {
		t.Fatalf("expected 1 AddTask call, got %d", len(tracker.addCalls))
	}
	call := tracker.addCalls[0]
	if call.phase != "Phase 1" {
		t.Errorf("phase not trimmed: %q", call.phase)
	}
	if call.feature != "Render" {
		t.Errorf("feature not trimmed: %q", call.feature)
	}
	if call.task != "draw" {
		t.Errorf("task not trimmed: %q", call.task)
	}
	if call.link != "/x.md" {
		t.Errorf("link not trimmed: %q", call.link)
	}
}

// TestProjectTaskAddTool_TrackerError verifies that a tracker-side
// failure (e.g. "no such phase") surfaces in the failure list
// without aborting the rest of the batch. Pairs the validation-
// driven partial-success path with the I/O-driven one.
func TestProjectTaskAddTool_TrackerError(t *testing.T) {
	tracker := &stubTracker{addErr: errors.New("no such phase")}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"tasks":[
		{"phase":"P","feature":"F","task":"t1"},
		{"phase":"P","feature":"F","task":"t2"}
	]}`
	result := tool.Execute(context.Background(), toolCall("test-id", "project_task_add", args))

	// Every entry calls AddTask and every call errors → all fail, none added.
	assertContains(t, result.Content, "no such phase")
	assertContains(t, result.Content, "Failed 2 task(s)")
	if strings.Contains(result.Content, "Added") {
		t.Errorf("no Added section expected when all entries fail: %q", result.Content)
	}
}
