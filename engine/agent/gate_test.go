package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/memory"
)

func newGateTestAgent(store memory.Store, mode Mode) *Agent {
	return &Agent{memoryStore: store, mode: mode}
}

func TestEnforceActiveTaskGate_NoMemoryStore(t *testing.T) {
	a := newGateTestAgent(nil, ModeExecution)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty, got: %s", msg)
	}
}

func TestEnforceActiveTaskGate_PlanningMode(t *testing.T) {
	// Even with a store and no active task, planning mode doesn't gate —
	// the planning-mode blocklist already rejects mutating tools.
	store := &mockStore{fetchDoc: memory.Document{Body: "# Phase 1: A\n## F\n- [ ] t\n"}}
	a := newGateTestAgent(store, ModePlanning)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty in planning mode, got: %s", msg)
	}
}

func TestEnforceActiveTaskGate_NonMutatingTool(t *testing.T) {
	store := &mockStore{fetchDoc: memory.Document{Body: "# Phase 1: A\n## F\n- [ ] t\n"}}
	a := newGateTestAgent(store, ModeExecution)
	for _, tool := range []string{"read_file", "search_project", "glob", "list_files"} {
		if msg := a.enforceActiveTaskGate(context.Background(), tool); msg != "" {
			t.Errorf("%s should not gate: %s", tool, msg)
		}
	}
}

func TestEnforceActiveTaskGate_FetchErrorAllows(t *testing.T) {
	// Fetch failure (e.g. not-found, server down) never blocks — avoids
	// hard-gating on infrastructure issues and fresh projects.
	store := &mockStore{fetchErr: memory.ErrNotFound}
	a := newGateTestAgent(store, ModeExecution)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty on fetch error, got: %s", msg)
	}
}

func TestEnforceActiveTaskGate_NoActiveTaskBlocks(t *testing.T) {
	store := &mockStore{fetchDoc: memory.Document{Body: "# Phase 1: A\n## F\n- [ ] pending only\n"}}
	a := newGateTestAgent(store, ModeExecution)
	for _, tool := range []string{"edit_file", "write_file", "bash"} {
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
	store := &mockStore{fetchDoc: memory.Document{Body: "# Phase 1: A\n## F\n- [>] active task\n"}}
	a := newGateTestAgent(store, ModeExecution)
	if msg := a.enforceActiveTaskGate(context.Background(), "edit_file"); msg != "" {
		t.Errorf("expected empty when active task present, got: %s", msg)
	}
}
