package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// initTracker is a controllable TaskTracker stub that records
// InitProject calls so tests can assert what the tool plumbed
// through to the session layer.
type initTracker struct {
	*stubTracker
	initCalls []struct {
		name   string
		phases []string
	}
	initErr error
}

func (t *initTracker) InitProject(name string, phases []string) error {
	t.initCalls = append(t.initCalls, struct {
		name   string
		phases []string
	}{name, append([]string(nil), phases...)})
	return t.initErr
}

// TestProjectInitTool_HappyPath verifies the tool plumbs the project
// name and phases through to the tracker, returns a useful summary
// the LLM can use for its next call, and surfaces an idempotency-safe
// success message.
func TestProjectInitTool_HappyPath(t *testing.T) {
	t.Parallel()

	tracker := &initTracker{stubTracker: &stubTracker{}}
	tool := NewProjectInitTool(tracker)

	args := mustMarshal(t, projectInitArgs{
		Name:   "Pac-Man Clone",
		Phases: []string{"Foundation", "Movement", "Ghosts"},
	})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "project_init", Arguments: string(args)},
	})

	if len(tracker.initCalls) != 1 {
		t.Fatalf("InitProject calls = %d, want 1", len(tracker.initCalls))
	}
	got := tracker.initCalls[0]
	if got.name != "Pac-Man Clone" {
		t.Errorf("InitProject name = %q, want %q", got.name, "Pac-Man Clone")
	}
	if len(got.phases) != 3 || got.phases[0] != "Foundation" {
		t.Errorf("InitProject phases = %v, want [Foundation Movement Ghosts]", got.phases)
	}
	if !strings.Contains(result.Content, "Pac-Man Clone") {
		t.Errorf("result missing project name; got %q", result.Content)
	}
	if !strings.Contains(result.Content, "project_task_add") {
		t.Errorf("result missing follow-up tool steer; got %q", result.Content)
	}
}

// TestProjectInitTool_NoPhases verifies the empty-phases case
// produces a message that steers the LLM toward adding phases by
// editing /project.md directly via memory tools — re-running
// project_init is a dead end (idempotent), and project_task_add
// alone will fail because the phase doesn't exist.
func TestProjectInitTool_NoPhases(t *testing.T) {
	t.Parallel()

	tracker := &initTracker{stubTracker: &stubTracker{}}
	tool := NewProjectInitTool(tracker)

	args := mustMarshal(t, projectInitArgs{Name: "Empty Project"})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "project_init", Arguments: string(args)},
	})

	if !strings.Contains(result.Content, "no phases") {
		t.Errorf("result missing no-phases marker: %q", result.Content)
	}
	if !strings.Contains(result.Content, "memory") {
		t.Errorf("result must steer LLM to memory tools for phase addition: %q", result.Content)
	}
	// Regression guard: prior wording said "Add phases by calling
	// project_init again" — InitProject is idempotent and ignores
	// new phases on a populated /project.md, so this guidance was a
	// dead end on fresh repos.
	if strings.Contains(result.Content, "project_init again") {
		t.Errorf("regression: no-phases message must not promise re-running project_init: %q",
			result.Content)
	}
	if got := tracker.initCalls[0].phases; len(got) != 0 {
		t.Errorf("phases = %v, want empty", got)
	}
}

// TestProjectInitTool_ValidatesArgs verifies the obvious error
// shapes — missing name, missing tracker, malformed JSON.
func TestProjectInitTool_ValidatesArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		tool       *ProjectInitTool
		args       string
		wantSubstr string
	}{
		{
			name:       "empty name",
			tool:       NewProjectInitTool(&initTracker{stubTracker: &stubTracker{}}),
			args:       `{"name":""}`,
			wantSubstr: "name is required",
		},
		{
			name:       "nil tracker",
			tool:       NewProjectInitTool(nil),
			args:       `{"name":"x"}`,
			wantSubstr: "task tracking not available",
		},
		{
			name:       "bad json",
			tool:       NewProjectInitTool(&initTracker{stubTracker: &stubTracker{}}),
			args:       `{`,
			wantSubstr: "invalid arguments",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.tool.Execute(context.Background(), llm.ToolCall{
				Function: llm.FunctionCall{Name: "project_init", Arguments: tc.args},
			})
			if !strings.Contains(result.Content, tc.wantSubstr) {
				t.Errorf("result missing %q: %q", tc.wantSubstr, result.Content)
			}
		})
	}
}

// TestProjectInitTool_SurfacesTrackerError verifies a failure from
// the underlying InitProject (memory unavailable, publish race) is
// surfaced as a tool error rather than swallowed.
func TestProjectInitTool_SurfacesTrackerError(t *testing.T) {
	t.Parallel()

	tracker := &initTracker{
		stubTracker: &stubTracker{},
		initErr:     errors.New("memory: connection refused"),
	}
	tool := NewProjectInitTool(tracker)
	args := mustMarshal(t, projectInitArgs{Name: "x", Phases: []string{"A"}})

	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "project_init", Arguments: string(args)},
	})
	if !strings.Contains(result.Content, "connection refused") {
		t.Errorf("tracker error not surfaced: %q", result.Content)
	}
}

// TestProjectInitTool_DefinitionMentionsIdempotent locks the operative
// safety hint that survives in the trimmed tool description: the LLM
// must know calling project_init twice will NOT clobber an existing
// /project.md. The wider "use project_init not memory_publish"
// disambiguation moved to the system prompt's Tool Notes — see the
// matching assertion in coding/agent/prompt_test.go where the prompt
// is the load-bearing carrier.
func TestProjectInitTool_DefinitionMentionsIdempotent(t *testing.T) {
	t.Parallel()

	def := NewProjectInitTool(&initTracker{stubTracker: &stubTracker{}}).Definition()

	if def.Function.Name != "project_init" {
		t.Errorf("Name = %q", def.Function.Name)
	}
	if !strings.Contains(def.Function.Description, "Idempotent") {
		t.Errorf("description missing %q: %q", "Idempotent", def.Function.Description)
	}
}
