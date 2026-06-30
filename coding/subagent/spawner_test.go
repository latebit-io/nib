package subagent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/toolperm"
)

// stubProvider is a non-functional llm.Provider used only as a sentinel
// (orchestration tests inject run and never stream).
type stubProvider struct{ id string }

func (stubProvider) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("stub provider does not stream")
}

type fakeTool struct{ name string }

func (f fakeTool) Definition() llm.ToolDef {
	return llm.ToolDef{Function: llm.FunctionDef{Name: f.name}}
}
func (f fakeTool) Execute(context.Context, llm.ToolCall) upagent.ToolResult {
	return upagent.ToolResult{}
}

func toolNames(tools []agent.Tool) []string {
	var n []string
	for _, t := range tools {
		n = append(n, t.Definition().Function.Name)
	}
	slices.Sort(n)
	return n
}

func mkMatcher(t *testing.T, allow, deny string) *toolperm.Matcher {
	t.Helper()
	a, err := toolperm.ParseField(allow)
	if err != nil {
		t.Fatal(err)
	}
	d, err := toolperm.ParseField(deny)
	if err != nil {
		t.Fatal(err)
	}
	return toolperm.New(a, d)
}

func TestGrantFilter(t *testing.T) {
	t.Parallel()
	tools := []agent.Tool{fakeTool{"Read"}, fakeTool{"Write"}, fakeTool{"Bash"}, fakeTool{"mcp_x"}}

	// Allow list: only granted tools survive.
	got := grantFilter(tools, mkMatcher(t, "Read Bash(git *)", ""))
	if want := []string{"Bash", "Read"}; !slices.Equal(toolNames(got), want) {
		t.Errorf("allow-list filter = %v, want %v", toolNames(got), want)
	}

	// No allow list: inherit all, minus an outright deny.
	got = grantFilter(tools, mkMatcher(t, "", "Write"))
	if want := []string{"Bash", "Read", "mcp_x"}; !slices.Equal(toolNames(got), want) {
		t.Errorf("inherit-minus-deny = %v, want %v", toolNames(got), want)
	}

	// Argument-scoped deny does not remove the tool.
	got = grantFilter(tools, mkMatcher(t, "Bash(git *)", "Bash(rm *)"))
	if want := []string{"Bash"}; !slices.Equal(toolNames(got), want) {
		t.Errorf("arg-deny should keep Bash = %v", toolNames(got))
	}
}

func TestProgressSink_PrefixesToolCalls(t *testing.T) {
	t.Parallel()
	var got []event.Event
	s := &Spawner{onEvent: func(ev event.Event) { got = append(got, ev) }}
	sink := s.progressSink("reviewer")

	sink(event.AgentToolCall{Name: "read_file", Args: "{}"})
	sink(event.AgentToken{Text: "noise"}) // non-tool-call dropped

	if len(got) != 1 {
		t.Fatalf("expected only the tool call forwarded, got %d", len(got))
	}
	tc, ok := got[0].(event.AgentToolCall)
	if !ok || tc.Name != "▸ reviewer: read_file" {
		t.Errorf("forwarded event = %+v", got[0])
	}

	if (&Spawner{}).progressSink("x") != nil {
		t.Errorf("no sink configured → nil progress sink")
	}
}

func TestComposeGoal(t *testing.T) {
	t.Parallel()
	withPersona := composeGoal(agentdef.Definition{SystemPrompt: "You are a reviewer."}, "check the diff")
	if withPersona != "You are a reviewer.\n\n---\n\nTask:\ncheck the diff" {
		t.Errorf("composeGoal persona = %q", withPersona)
	}
	if got := composeGoal(agentdef.Definition{}, "just the task"); got != "just the task" {
		t.Errorf("empty persona should pass task through, got %q", got)
	}
}

func TestSpawn_Orchestration(t *testing.T) {
	t.Parallel()
	var gotProv llm.Provider
	var gotTools []agent.Tool
	var gotGoal string
	s := &Spawner{
		provider:  func() llm.Provider { return stubProvider{id: "parent"} },
		baseTools: []agent.Tool{fakeTool{"Read"}, fakeTool{"Write"}},
		budget:    5,
		run: func(_ context.Context, p llm.Provider, _ *headless.DiskWorkspace, opts *agent.NewOptions, tools []agent.Tool, goal string, _ func(event.Event)) (headless.Result, error) {
			gotProv, gotTools, gotGoal = p, tools, goal
			if opts.Interaction != agent.Headless || !opts.Terse || opts.TaskTokenBudget != 5 {
				t.Errorf("opts wrong: %+v", opts)
			}
			return headless.Result{Success: true, Summary: "ok"}, nil
		},
	}
	// An unrestricted definition inherits all base tools (grant-filtering
	// is unit-tested separately; a restricted def is rejected — see
	// TestSpawn_RejectsUnenforceableGrants).
	def := agentdef.Definition{Name: "rev", SystemPrompt: "persona"}
	if _, err := s.Spawn(context.Background(), def, "do it"); err != nil {
		t.Fatal(err)
	}
	if gotProv.(stubProvider).id != "parent" {
		t.Errorf("expected parent provider")
	}
	if want := []string{"Read", "Write"}; !slices.Equal(toolNames(gotTools), want) {
		t.Errorf("tools = %v, want %v", toolNames(gotTools), want)
	}
	if gotGoal != "persona\n\n---\n\nTask:\ndo it" {
		t.Errorf("goal = %q", gotGoal)
	}
}

func TestSpawn_RejectsUnenforceableGrants(t *testing.T) {
	t.Parallel()
	s := &Spawner{
		provider: func() llm.Provider { return stubProvider{} },
		run: func(context.Context, llm.Provider, *headless.DiskWorkspace, *agent.NewOptions, []agent.Tool, string, func(event.Event)) (headless.Result, error) {
			t.Errorf("run must not be called for a restricted definition")
			return headless.Result{}, nil
		},
	}
	// A definition that restricts tools cannot be honored against builtins
	// yet, so it is refused rather than run with a false boundary.
	for _, def := range []agentdef.Definition{
		{Name: "a", SystemPrompt: "p", AllowedTools: []string{"Read"}},
		{Name: "b", SystemPrompt: "p", DisallowedTools: []string{"Write"}},
	} {
		if _, err := s.Spawn(context.Background(), def, "t"); err == nil {
			t.Errorf("def %q: expected rejection of unenforceable tool restriction", def.Name)
		}
	}
}

func TestSpawn_ModelOverride(t *testing.T) {
	t.Parallel()
	s := &Spawner{
		provider:    func() llm.Provider { return stubProvider{id: "parent"} },
		providerFor: func(model string) (llm.Provider, error) { return stubProvider{id: model}, nil },
		run: func(_ context.Context, p llm.Provider, _ *headless.DiskWorkspace, _ *agent.NewOptions, _ []agent.Tool, _ string, _ func(event.Event)) (headless.Result, error) {
			return headless.Result{Success: true, Summary: p.(stubProvider).id}, nil
		},
	}
	res, err := s.Spawn(context.Background(), agentdef.Definition{Name: "x", SystemPrompt: "p", Model: "fast-model"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "fast-model" {
		t.Errorf("model override not applied, provider id = %q", res.Summary)
	}

	// Model requested but no resolver → error.
	s.providerFor = nil
	if _, err := s.Spawn(context.Background(), agentdef.Definition{Name: "x", SystemPrompt: "p", Model: "m"}, "t"); err == nil {
		t.Errorf("expected error when a model is requested with no resolver")
	}
}

func TestSpawn_WorktreeIsolation(t *testing.T) {
	t.Parallel()
	parent := headless.NewDiskWorkspace(t.TempDir())
	wtRoot := t.TempDir()
	cleanupCalled := false
	var ranIn string

	s := &Spawner{
		provider:  func() llm.Provider { return stubProvider{} },
		workspace: parent,
		worktree: func(_ context.Context, repo string) (string, func(), error) {
			if repo != parent.ProjectRoot() {
				t.Errorf("worktree repo = %q, want parent root %q", repo, parent.ProjectRoot())
			}
			return wtRoot, func() { cleanupCalled = true }, nil
		},
		run: func(_ context.Context, _ llm.Provider, ws *headless.DiskWorkspace, _ *agent.NewOptions, _ []agent.Tool, _ string, _ func(event.Event)) (headless.Result, error) {
			ranIn = ws.ProjectRoot()
			return headless.Result{Success: true}, nil
		},
	}

	// Isolated: child runs in the worktree root and cleanup fires.
	iso := agentdef.Definition{Name: "x", SystemPrompt: "p", Isolation: agentdef.IsolationWorktree}
	if _, err := s.Spawn(context.Background(), iso, "t"); err != nil {
		t.Fatal(err)
	}
	if ranIn != wtRoot {
		t.Errorf("isolated child ran in %q, want worktree %q", ranIn, wtRoot)
	}
	if !cleanupCalled {
		t.Errorf("worktree cleanup must be called")
	}

	// Non-isolated: child runs in the parent workspace, no worktree.
	cleanupCalled = false
	plain := agentdef.Definition{Name: "y", SystemPrompt: "p"}
	if _, err := s.Spawn(context.Background(), plain, "t"); err != nil {
		t.Fatal(err)
	}
	if ranIn != parent.ProjectRoot() {
		t.Errorf("non-isolated child should run in parent root, got %q", ranIn)
	}
	if cleanupCalled {
		t.Errorf("no worktree should be created for a non-isolated agent")
	}
}

func TestSpawn_WorktreeError(t *testing.T) {
	t.Parallel()
	s := &Spawner{
		provider:  func() llm.Provider { return stubProvider{} },
		workspace: headless.NewDiskWorkspace(t.TempDir()),
		worktree: func(context.Context, string) (string, func(), error) {
			return "", nil, errors.New("not a git repo")
		},
		run: func(context.Context, llm.Provider, *headless.DiskWorkspace, *agent.NewOptions, []agent.Tool, string, func(event.Event)) (headless.Result, error) {
			t.Errorf("run must not be called when the worktree cannot be created")
			return headless.Result{}, nil
		},
	}
	iso := agentdef.Definition{Name: "x", SystemPrompt: "p", Isolation: agentdef.IsolationWorktree}
	if _, err := s.Spawn(context.Background(), iso, "t"); err == nil {
		t.Errorf("expected error when worktree creation fails")
	}
}

func TestSpawnTool_Execute(t *testing.T) {
	t.Parallel()
	makeSpawner := func(res headless.Result, err error) *Spawner {
		return &Spawner{
			provider: func() llm.Provider { return stubProvider{} },
			run: func(context.Context, llm.Provider, *headless.DiskWorkspace, *agent.NewOptions, []agent.Tool, string, func(event.Event)) (headless.Result, error) {
				return res, err
			},
		}
	}
	def := agentdef.Definition{Name: "rev", Description: "reviews", SystemPrompt: "p"}

	tool := AdaptTool(def, makeSpawner(headless.Result{Success: true, Summary: "looks good"}, nil))
	if tool.Definition().Function.Name != "agent_rev" {
		t.Errorf("tool name = %q", tool.Definition().Function.Name)
	}
	out := tool.Execute(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Arguments: `{"task":"review it"}`}})
	if out.IsError || out.Content != "looks good" {
		t.Errorf("success result = %+v", out)
	}

	// Missing task → error, no spawn.
	bad := tool.Execute(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Arguments: `{}`}})
	if !bad.IsError {
		t.Errorf("missing task should be an error")
	}

	// Failed run → IsError with summary surfaced.
	failTool := AdaptTool(def, makeSpawner(headless.Result{Success: false, Summary: "partial", Errors: []string{"boom"}}, nil))
	fr := failTool.Execute(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Arguments: `{"task":"x"}`}})
	if !fr.IsError || !strings.Contains(fr.Content, "boom") {
		t.Errorf("failed run = %+v", fr)
	}
}
