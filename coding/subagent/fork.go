package subagent

import (
	"context"
	"slices"
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
		skill:   s,
		spawner: spawner,
	}
}

// forkDefinition derives the child-agent spec from a forking skill. The
// body is the TASK (not the persona), so SystemPrompt is left empty; the
// skill's grants flow through as the child's tool filter.
func forkDefinition(s skill.Skill) agentdef.Definition {
	allowed := s.AllowedTools
	// An allow list would otherwise filter out the reader for the
	// skill's own bundled files.
	if len(allowed) > 0 {
		allowed = append(slices.Clone(allowed), skill.ResourceToolName)
	}
	return agentdef.Definition{
		Name:            s.Name,
		Description:     s.Description,
		AllowedTools:    allowed,
		DisallowedTools: s.DisallowedTools,
		Source:          agentdef.Source(string(s.Source)),
		PluginID:        s.PluginID,
	}
}

// forkSkillTool is the agent tool for a forking skill. Unlike the prompt
// skill tool, Execute spawns a child agent instead of returning text.
type forkSkillTool struct {
	def     llm.ToolDef
	skill   skill.Skill
	spawner *Spawner
}

// Definition returns the advertised schema (no arguments — the skill body
// is the task).
func (t forkSkillTool) Definition() llm.ToolDef { return t.def }

// Execute runs the skill body, with its bundled-file note, as a child
// agent and returns its summary.
func (t forkSkillTool) Execute(ctx context.Context, _ llm.ToolCall) upagent.ToolResult {
	task := t.skill.Body
	var extra []upagent.Tool
	if note := t.skill.ResourceNote(skill.ResourceToolName); note != "" {
		task += note
		extra = append(extra, skill.ResourceTool(t.skill))
	}
	res, err := t.spawner.Spawn(ctx, forkDefinition(t.skill), task, extra...)
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
