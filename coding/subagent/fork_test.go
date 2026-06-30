package subagent

import (
	"context"
	"slices"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	"github.com/latebit-io/nib/kit/skill"
)

func TestAdaptForkSkill_SpawnsBodyAsTask(t *testing.T) {
	t.Parallel()
	var gotTools []agent.Tool
	var gotGoal string
	s := &Spawner{
		parentProv: stubProvider{},
		baseTools:  []agent.Tool{fakeTool{"Read"}, fakeTool{"Write"}},
		run: func(_ context.Context, _ llm.Provider, _ *headless.DiskWorkspace, _ *agent.NewOptions, tools []agent.Tool, goal string, _ func(event.Event)) (headless.Result, error) {
			gotTools, gotGoal = tools, goal
			return headless.Result{Success: true, Summary: "forked-result"}, nil
		},
	}
	sk := skill.Skill{
		Name:         "f",
		Description:  "forks",
		Body:         "do the work",
		AllowedTools: []string{"Read"},
		Context:      "fork",
		Source:       skill.SourceProject,
	}

	tool := AdaptForkSkill(sk, s)
	if tool.Definition().Function.Name != "skill_f" {
		t.Errorf("fork skill tool name = %q, want skill_f", tool.Definition().Function.Name)
	}
	// No arguments — the body is the task.
	if len(tool.Definition().Function.Parameters.Properties) != 0 {
		t.Errorf("fork skill tool should take no arguments")
	}

	out := tool.Execute(context.Background(), llm.ToolCall{})
	if out.IsError || out.Content != "forked-result" {
		t.Errorf("execute = %+v", out)
	}
	// The skill body is the child's goal (persona empty), grant-filtered to Read.
	if gotGoal != "do the work" {
		t.Errorf("child goal = %q, want the skill body", gotGoal)
	}
	if want := []string{"Read"}; !slices.Equal(toolNames(gotTools), want) {
		t.Errorf("child tools = %v, want %v (grant-filtered)", toolNames(gotTools), want)
	}
}

func TestForkDefinition_MapsProvenance(t *testing.T) {
	t.Parallel()
	d := forkDefinition(skill.Skill{
		Name: "x", Description: "d", Body: "b",
		AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"},
		Source: skill.SourcePlugin, PluginID: "demo",
	})
	if d.SystemPrompt != "" {
		t.Errorf("persona must be empty (body is the task), got %q", d.SystemPrompt)
	}
	if string(d.Source) != "plugin" || d.PluginID != "demo" {
		t.Errorf("provenance not mapped: %+v", d)
	}
	if !d.Permissions().GrantsTool("Read") || d.Permissions().DeniesTool("Read") {
		t.Errorf("grants not carried")
	}
}
