package agent

import (
	"context"
	"strings"
	"testing"
)

// gateTracker is a TaskTracker stub that drives enforceActiveTaskGate
// through its loaded/active combinations.
type gateTracker struct {
	loaded     bool
	activePath string
}

func (g *gateTracker) ActivateTask(string) error          { return nil }
func (g *gateTracker) CompleteTask(string) error          { return nil }
func (g *gateTracker) AddTask(_, _, _, _ string) error    { return nil }
func (g *gateTracker) ActiveTaskPath() string             { return g.activePath }
func (g *gateTracker) WorkTreeLoaded() bool               { return g.loaded }
func (g *gateTracker) NextPendingTask() string            { return "" }
func (g *gateTracker) InitProject(string, []string) error { return nil }

// gateTestWorkspace wraps gateTracker so it satisfies both Workspace
// (via an embedded testWorkspace) and TaskTracker.
type gateTestWorkspace struct {
	*testWorkspace
	*gateTracker
}

func newGateTestAgent(tracker *gateTracker, mode Mode) *Agent {
	ws := &gateTestWorkspace{
		testWorkspace: &testWorkspace{},
		gateTracker:   tracker,
	}
	return &Agent{workspace: ws, mode: mode}
}

func TestEnforceActiveTaskGate_NoTaskTracker(t *testing.T) {
	// Workspace that doesn't implement TaskTracker — gate stays off.
	a := &Agent{workspace: &testWorkspace{}, mode: ModeExecution}
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty, got: %s", msg)
	}
}

func TestEnforceActiveTaskGate_PlanningMode(t *testing.T) {
	// Planning mode doesn't gate — the planning-mode blocklist already
	// rejects mutating tools.
	tracker := &gateTracker{loaded: true, activePath: ""}
	a := newGateTestAgent(tracker, ModePlanning)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty in planning mode, got: %s", msg)
	}
}

func TestEnforceActiveTaskGate_NonMutatingTool(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: ""}
	a := newGateTestAgent(tracker, ModeExecution)
	for _, tool := range []string{"read_file", "search_project", "glob", "list_files"} {
		if msg := a.enforceActiveTaskGate(context.Background(), tool); msg != "" {
			t.Errorf("%s should not gate: %s", tool, msg)
		}
	}
}

func TestEnforceActiveTaskGate_TreeNotLoadedAllows(t *testing.T) {
	// Tree not loaded covers both the fresh-project ErrNotFound case and
	// the "demarkus unreachable at startup" case. Failing open here is
	// what prevents the startup deadlock: the gate and the task tools
	// used to both require a loaded tree, leaving no escape path.
	tracker := &gateTracker{loaded: false}
	a := newGateTestAgent(tracker, ModeExecution)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty when tree not loaded, got: %s", msg)
	}
}

func TestEnforceActiveTaskGate_NoActiveTaskBlocks(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: ""}
	a := newGateTestAgent(tracker, ModeExecution)
	for _, tool := range []string{"edit_file", "write_file", "replace_file", "bash", "smoke_run"} {
		msg := a.enforceActiveTaskGate(context.Background(), tool)
		if msg == "" {
			t.Errorf("%s should have been blocked", tool)
			continue
		}
		if !strings.Contains(msg, "no active task") {
			t.Errorf("%s: unexpected message: %s", tool, msg)
		}
		if !strings.Contains(msg, "update_task") || !strings.Contains(msg, "project_task_add") {
			t.Errorf("%s: message should direct agent to the task tools: %s", tool, msg)
		}
	}
}

func TestEnforceActiveTaskGate_ActiveTaskPasses(t *testing.T) {
	tracker := &gateTracker{loaded: true, activePath: "Phase 1 > Setup > task"}
	a := newGateTestAgent(tracker, ModeExecution)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty when active task present, got: %s", msg)
	}
}
