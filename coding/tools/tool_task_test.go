package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTaskTracker is a minimal TaskTracker stub. Records the calls so a
// test can assert on which side effects fired and in what order.
type fakeTaskTracker struct {
	next        string // value NextPendingTask returns
	activateErr error  // returned from ActivateTask
	completeErr error  // returned from CompleteTask

	activated   []string // titles passed to ActivateTask, in order
	completed   []string // titles passed to CompleteTask, in order
	nextQueried int      // number of NextPendingTask calls
}

func (f *fakeTaskTracker) ActiveTaskPath() string { return "" }
func (f *fakeTaskTracker) WorkTreeLoaded() bool   { return true }
func (f *fakeTaskTracker) NextPendingTask() string {
	f.nextQueried++
	return f.next
}

func (f *fakeTaskTracker) ActivateTask(title string) error {
	f.activated = append(f.activated, title)
	return f.activateErr
}

func (f *fakeTaskTracker) CompleteTask(title string) error {
	f.completed = append(f.completed, title)
	return f.completeErr
}

func (f *fakeTaskTracker) AddTask(_, _, _, _ string) error { return nil }
func (f *fakeTaskTracker) InitProject(_ string, _ []string) error {
	return nil
}

// echoReviewer returns the base message unchanged so test assertions can
// scope to the auto-activate suffix without coupling to real reviewer
// output.
type echoReviewer struct{}

func (echoReviewer) OnComplete(_ context.Context, base string) string { return base }

func TestUpdateTask_CompleteAutoActivatesNext(t *testing.T) {
	tracker := &fakeTaskTracker{next: "Implement movement"}
	tool := NewTaskTool(tracker, echoReviewer{})

	res := tool.Execute(context.Background(), toolCall("c1", "update_task",
		`{"title":"Scaffold project","action":"complete"}`))

	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if got := tracker.completed; len(got) != 1 || got[0] != "Scaffold project" {
		t.Errorf("CompleteTask calls = %v, want [\"Scaffold project\"]", got)
	}
	if got := tracker.activated; len(got) != 1 || got[0] != "Implement movement" {
		t.Errorf("ActivateTask calls = %v, want [\"Implement movement\"]", got)
	}
	assertContains(t, res.Content, "Task completed: Scaffold project")
	assertContains(t, res.Content, "Next task auto-activated: Implement movement")
}

func TestUpdateTask_CompleteWithNoMoreTasks(t *testing.T) {
	// NextPendingTask returns "" → tool should report all-complete and
	// NOT call ActivateTask (nothing to activate).
	tracker := &fakeTaskTracker{next: ""}
	tool := NewTaskTool(tracker, echoReviewer{})

	res := tool.Execute(context.Background(), toolCall("c1", "update_task",
		`{"title":"Last task","action":"complete"}`))

	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if len(tracker.activated) != 0 {
		t.Errorf("ActivateTask should not be called when no pending tasks remain; got %v", tracker.activated)
	}
	assertContains(t, res.Content, "All tasks complete.")
}

func TestUpdateTask_CompleteAutoActivateFailsGracefully(t *testing.T) {
	// Activation failure (e.g. demarkus write rejected) must NOT undo
	// the successful complete. Surface the failure inline so the LLM
	// can retry, but keep the complete persisted.
	activateErr := errors.New("demarkus: version conflict")
	tracker := &fakeTaskTracker{next: "Implement movement", activateErr: activateErr}
	tool := NewTaskTool(tracker, echoReviewer{})

	res := tool.Execute(context.Background(), toolCall("c1", "update_task",
		`{"title":"Scaffold","action":"complete"}`))

	if res.IsError {
		t.Fatalf("complete itself should not error when only activate fails: %+v", res)
	}
	if len(tracker.completed) != 1 {
		t.Errorf("CompleteTask should still have fired; got %v", tracker.completed)
	}
	assertContains(t, res.Content, "Task completed: Scaffold")
	assertContains(t, res.Content, "auto-activate failed")
	assertContains(t, res.Content, "demarkus: version conflict")
}

func TestUpdateTask_ActivateDoesNotChain(t *testing.T) {
	// Auto-activation is scoped to the complete path. An explicit
	// activate must NOT trigger another auto-activate — the LLM picked
	// a specific task on purpose; chaining would skip it.
	tracker := &fakeTaskTracker{next: "Other task"}
	tool := NewTaskTool(tracker, echoReviewer{})

	res := tool.Execute(context.Background(), toolCall("c1", "update_task",
		`{"title":"Picked task","action":"activate"}`))

	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	if got := tracker.activated; len(got) != 1 || got[0] != "Picked task" {
		t.Errorf("ActivateTask calls = %v, want [\"Picked task\"]", got)
	}
	if tracker.nextQueried != 0 {
		t.Errorf("activate path queried NextPendingTask %d times; want 0 (no chaining)", tracker.nextQueried)
	}
	if strings.Contains(res.Content, "Next task auto-activated") {
		t.Errorf("activate result must not advertise auto-activation, got: %q", res.Content)
	}
}

func TestUpdateTask_CompleteFailureSkipsAutoActivate(t *testing.T) {
	// If CompleteTask itself errors (e.g. title doesn't match), we
	// must NOT activate something else — the LLM's stated intent
	// failed, so the tree state should be unchanged.
	tracker := &fakeTaskTracker{
		next:        "Should not be touched",
		completeErr: errors.New("no such task"),
	}
	tool := NewTaskTool(tracker, echoReviewer{})

	res := tool.Execute(context.Background(), toolCall("c1", "update_task",
		`{"title":"Bogus","action":"complete"}`))

	if !res.IsError {
		t.Fatalf("expected error result for failed complete; got %+v", res)
	}
	if len(tracker.activated) != 0 {
		t.Errorf("ActivateTask must not fire when CompleteTask fails; got %v", tracker.activated)
	}
	if tracker.nextQueried != 0 {
		t.Errorf("NextPendingTask must not be consulted after a failed complete; got %d", tracker.nextQueried)
	}
}

func TestUpdateTask_CompleteRunsReviewerBeforeAutoActivate(t *testing.T) {
	// Reviewer output must appear BEFORE the activation notice — the
	// LLM should see lint/smoke findings prominently, with the
	// "what's next" hint as a trailing line. A reviewer that prepends
	// findings to the base message lets us assert the ordering
	// without a real reviewer.
	tracker := &fakeTaskTracker{next: "Next thing"}
	rev := prefixReviewer{prefix: "[review findings] "}
	tool := NewTaskTool(tracker, rev)

	res := tool.Execute(context.Background(), toolCall("c1", "update_task",
		`{"title":"Done thing","action":"complete"}`))

	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	reviewIdx := strings.Index(res.Content, "[review findings]")
	activateIdx := strings.Index(res.Content, "Next task auto-activated")
	if reviewIdx < 0 || activateIdx < 0 {
		t.Fatalf("missing expected segments in %q", res.Content)
	}
	if reviewIdx > activateIdx {
		t.Errorf("review output should appear before auto-activate notice; got:\n%s", res.Content)
	}
}

// prefixReviewer prepends a fixed marker to the base message so a test
// can assert reviewer output ordering relative to auto-activation.
type prefixReviewer struct{ prefix string }

func (r prefixReviewer) OnComplete(_ context.Context, base string) string {
	return r.prefix + base
}
