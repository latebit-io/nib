package subagent

import (
	"context"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/skill"
)

// AdaptForkSkill turns a `context: fork` skill into a spawn tool. The
// model invokes it like any skill (skill_<name>, no arguments); instead
// of injecting the body as prompt text, nib runs the body as an isolated
// child agent under the skill's own tool grants and returns the child's
// summary. The skill body becomes the child's task; the skill grants
// become the child's tool filter.
//
// A fork skill always runs an agent built from the skill itself: the
// skill loader rejects a non-empty `agent:` reference (resolving a named
// subagent type is a later refinement), so there is never a target to
// honor here.
func AdaptForkSkill(s skill.Skill, spawner *Spawner) upagent.Tool {
	return forkSkillTool{
		def: llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        skill.ToolNamePrefix + s.Name,
				Description: s.Description,
				Parameters: llm.FunctionParams{
					Type:       "object",
					Properties: map[string]llm.FunctionParam{},
				},
			},
		},
		agentDef: forkDefinition(s),
		body:     s.Body,
		spawner:  spawner,
	}
}

// forkDefinition derives the child-agent spec from a forking skill. The
// body is the TASK (not the persona), so SystemPrompt is left empty; the
// skill's grants flow through as the child's tool filter.
func forkDefinition(s skill.Skill) agentdef.Definition {
	return agentdef.Definition{
		Name:            s.Name,
		Description:     s.Description,
		AllowedTools:    s.AllowedTools,
		DisallowedTools: s.DisallowedTools,
		Source:          agentdef.Source(string(s.Source)),
		PluginID:        s.PluginID,
	}
}

// forkSkillTool is the agent tool for a forking skill. Unlike the prompt
// skill tool, Execute spawns a child agent instead of returning text.
type forkSkillTool struct {
	def      llm.ToolDef
	agentDef agentdef.Definition
	body     string
	spawner  *Spawner
}

// Definition returns the advertised schema (no arguments — the skill body
// is the task).
func (t forkSkillTool) Definition() llm.ToolDef { return t.def }

// Execute runs the skill body as a child agent and returns its summary.
func (t forkSkillTool) Execute(ctx context.Context, _ llm.ToolCall) upagent.ToolResult {
	res, err := t.spawner.Spawn(ctx, t.agentDef, t.body)
	if err != nil {
		return upagent.ToolResult{Content: "fork skill error: " + err.Error(), IsError: true}
	}
	content := strings.TrimSpace(res.Summary)
	if !res.Success {
		if len(res.Errors) > 0 {
			content += "\n\nerrors:\n" + strings.Join(res.Errors, "\n")
		}
		if content == "" {
			content = "forked skill did not complete successfully"
		}
		return upagent.ToolResult{Content: content, IsError: true}
	}
	return upagent.ToolResult{Content: content}
}
