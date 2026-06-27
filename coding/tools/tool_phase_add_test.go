package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestProjectPhaseAddTool_BatchHappyPath verifies the tool plumbs each
// title through AddPhase in order and reports the full numbered titles
// the tracker returned (so the LLM can target them with project_task_add).
func TestProjectPhaseAddTool_BatchHappyPath(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectPhaseAddTool(tracker)

	args := `{"phases": ["Polish", "Release"]}`
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", args))

	if want := []string{"Polish", "Release"}; len(tracker.phaseCalls) != 2 ||
		tracker.phaseCalls[0] != want[0] || tracker.phaseCalls[1] != want[1] {
		t.Fatalf("phaseCalls = %v, want %v", tracker.phaseCalls, want)
	}
	for _, want := range []string{"Added 2 phase(s)", "Phase 1: Polish", "Phase 2: Release"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("result missing %q:\n%s", want, result.Content)
		}
	}
}

// TestProjectPhaseAddTool_PartialSuccess verifies a tracker rejection on
// one entry does not stop the others, and the failure is reported inline.
func TestProjectPhaseAddTool_PartialSuccess(t *testing.T) {
	tracker := &stubTracker{phaseErrFn: func(title string) error {
		if title == "Dup" {
			return errors.New(`phase "Phase 1: Dup" already exists`)
		}
		return nil
	}}
	tool := NewProjectPhaseAddTool(tracker)

	args := `{"phases": ["Polish", "Dup", "Release"]}`
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", args))

	if !strings.Contains(result.Content, "Added 2 phase(s)") {
		t.Errorf("result missing added section:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "Failed 1 phase(s)") ||
		!strings.Contains(result.Content, "already exists") {
		t.Errorf("result missing failure section:\n%s", result.Content)
	}
}

// TestProjectPhaseAddTool_WithinBatchDuplicate verifies a case-folded
// repeat in the same call is reported as a duplicate and AddPhase is
// called only once — guarding against auto-numbering turning an LLM
// slip into two near-identical phases.
func TestProjectPhaseAddTool_WithinBatchDuplicate(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectPhaseAddTool(tracker)

	args := `{"phases": ["Polish", "polish"]}`
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", args))

	if len(tracker.phaseCalls) != 1 {
		t.Fatalf("phaseCalls = %v, want exactly 1 (second is a dup)", tracker.phaseCalls)
	}
	if !strings.Contains(result.Content, "duplicate of phases[0]") {
		t.Errorf("result missing within-batch dup line:\n%s", result.Content)
	}
}

// TestProjectPhaseAddTool_WithinBatchDuplicateMixedForm locks the
// bug-prone path: a bare title and an explicitly-numbered form of the
// same descriptive phase ("Polish" vs "Phase 9: Polish") must collide.
// The dedup key strips the "Phase N:" prefix, so only one AddPhase fires
// and the numbered entry is reported as the duplicate.
func TestProjectPhaseAddTool_WithinBatchDuplicateMixedForm(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectPhaseAddTool(tracker)

	args := `{"phases": ["Polish", "Phase 9: Polish"]}`
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", args))

	if len(tracker.phaseCalls) != 1 {
		t.Fatalf("phaseCalls = %v, want exactly 1 (numbered form is the same phase)", tracker.phaseCalls)
	}
	if tracker.phaseCalls[0] != "Polish" {
		t.Errorf("first added = %q, want %q", tracker.phaseCalls[0], "Polish")
	}
	if !strings.Contains(result.Content, "duplicate of phases[0]") {
		t.Errorf("result missing within-batch dup line for numbered form:\n%s", result.Content)
	}
}

// TestProjectPhaseAddTool_EmptyTitle verifies a blank entry is rejected
// without reaching the tracker.
func TestProjectPhaseAddTool_EmptyTitle(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewProjectPhaseAddTool(tracker)

	args := `{"phases": ["  "]}`
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", args))

	if len(tracker.phaseCalls) != 0 {
		t.Errorf("phaseCalls = %v, want none", tracker.phaseCalls)
	}
	if !strings.Contains(result.Content, "title is required") {
		t.Errorf("result missing empty-title failure:\n%s", result.Content)
	}
}

// TestProjectPhaseAddTool_EmptyArray verifies the empty-batch guard.
func TestProjectPhaseAddTool_EmptyArray(t *testing.T) {
	tool := NewProjectPhaseAddTool(&stubTracker{})
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", `{"phases": []}`))
	if !strings.Contains(result.Content, "must not be empty") {
		t.Errorf("result missing empty-array error:\n%s", result.Content)
	}
}

// TestProjectPhaseAddTool_NilTracker verifies the tool degrades to a
// clear error when task tracking is unavailable.
func TestProjectPhaseAddTool_NilTracker(t *testing.T) {
	tool := NewProjectPhaseAddTool(nil)
	result := tool.Execute(context.Background(), toolCall("id", "project_phase_add", `{"phases": ["X"]}`))
	if !strings.Contains(result.Content, "task tracking not available") {
		t.Errorf("result missing nil-tracker error:\n%s", result.Content)
	}
}
