package agent

import (
	"context"
	"errors"
	"testing"
)

// stubTracker captures AddTask calls for assertion.
type stubTracker struct {
	addCalls []struct{ phase, feature, task, link string }
	addErr   error
}

func (s *stubTracker) ActivateTask(string) error { return nil }
func (s *stubTracker) CompleteTask(string) error { return nil }
func (s *stubTracker) ActiveTaskPath() string    { return "" }
func (s *stubTracker) WorkTreeLoaded() bool      { return true }
func (s *stubTracker) AddTask(p, f, t, l string) error {
	s.addCalls = append(s.addCalls, struct{ phase, feature, task, link string }{p, f, t, l})
	return s.addErr
}

func TestProjectTaskAddTool_Success(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	args := `{"phase": "Phase 1", "feature": "Render", "task": "draw sprites", "link": "/game/sprites.md"}`
	result := tool.Execute(context.Background(), toolCall("project_task_add", args))

	assertContains(t, result.Content, "Added: Phase 1 > Render > draw sprites")
	if len(tracker.addCalls) != 1 {
		t.Fatalf("expected 1 AddTask call, got %d", len(tracker.addCalls))
	}
	call := tracker.addCalls[0]
	if call.phase != "Phase 1" || call.feature != "Render" ||
		call.task != "draw sprites" || call.link != "/game/sprites.md" {
		t.Errorf("wrong args passed to tracker: %+v", call)
	}
}

func TestProjectTaskAddTool_Validation(t *testing.T) {
	tool := NewProjectTaskAddTool(&stubTracker{})
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing phase", `{"feature": "F", "task": "t"}`, "phase is required"},
		{"missing feature", `{"phase": "P", "task": "t"}`, "feature is required"},
		{"missing task", `{"phase": "P", "feature": "F"}`, "task is required"},
		{"bad json", `{not json`, "invalid arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), toolCall("project_task_add", tc.args))
			assertContains(t, result.Content, tc.want)
		})
	}
}

func TestProjectTaskAddTool_NilTracker(t *testing.T) {
	tool := NewProjectTaskAddTool(nil)
	args := `{"phase": "P", "feature": "F", "task": "t"}`
	result := tool.Execute(context.Background(), toolCall("project_task_add", args))
	assertContains(t, result.Content, "task tracking not available")
}

func TestProjectTaskAddTool_NormalizesWhitespace(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectTaskAddTool(tracker)
	// Whitespace-padded args must reach the tracker trimmed so lookups
	// against "Phase 1" / "Render" succeed.
	args := `{"phase": "  Phase 1  ", "feature": "\tRender\n", "task": "  draw  ", "link": "  /x.md  "}`
	result := tool.Execute(context.Background(), toolCall("project_task_add", args))
	assertContains(t, result.Content, "Added: Phase 1 > Render > draw")

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

func TestProjectTaskAddTool_TrackerError(t *testing.T) {
	tool := NewProjectTaskAddTool(&stubTracker{addErr: errors.New("no such phase")})
	args := `{"phase": "P", "feature": "F", "task": "t"}`
	result := tool.Execute(context.Background(), toolCall("project_task_add", args))
	assertContains(t, result.Content, "no such phase")
}
