package skill

import (
	"context"
	"fmt"
	"log/slog"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// ToolNamePrefix namespaces skill tool names so a skill can never
// shadow (and be silently dropped by) a built-in tool — built-ins are
// registered first and win on name collision. Exported so the prompt
// layer can detect skill tools without re-deriving the convention.
const ToolNamePrefix = "skill_"

// skillTool adapts a pure-prompt [Skill] into an [agent.Tool]. The
// description is advertised in the tool list; Execute returns the body
// so the instructions load only when the model invokes the skill.
type skillTool struct {
	def  llm.ToolDef
	body string
}

// Definition returns the advertised schema: the skill's description and
// an empty-object parameter set (invoking a skill takes no arguments —
// it pulls the instructions, then the model continues).
func (t skillTool) Definition() llm.ToolDef { return t.def }

// Execute returns the skill body verbatim as the tool result. It never
// fails and ignores the call arguments — the skill is pure instruction
// text, with no side effects.
func (t skillTool) Execute(_ context.Context, _ llm.ToolCall) upagent.ToolResult {
	return upagent.ToolResult{Content: t.body}
}

// adaptTool builds the agent.Tool for a pure-prompt skill.
func adaptTool(s Skill) skillTool {
	return skillTool{
		def: llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        ToolNamePrefix + s.Name,
				Description: s.Description,
				Parameters: llm.FunctionParams{
					Type:       "object",
					Properties: map[string]llm.FunctionParam{},
				},
			},
		},
		body: s.Body,
	}
}

// Result is the outcome of [Discover]: the adapted tools plus the names
// of skills that loaded and skills that were refused, so the wiring
// site can surface both (e.g. via --plugins).
type Result struct {
	// Tools are the agent-compatible adapters for supported skills,
	// ready to pass as extra tools at agent construction.
	Tools []upagent.Tool
	// Loaded names the skills adapted into Tools.
	Loaded []string
	// Skipped names the skills refused in v1 (script-bearing), with the
	// reason logged. Surfaced so the refusal is visible, not silent.
	Skipped []string
}

// Discover loads skills from root and adapts the supported ones into
// tools. Script-bearing skills ([Skill.NeedsShell]) are refused with a
// logged warning and recorded in Result.Skipped rather than adapted —
// executing third-party shell needs the (unbuilt) bash-approval
// surface. Replacing this branch with a script-skill adapter is the
// whole of the future extension; nothing else here changes.
//
// A non-nil error reports per-skill parse failures (see [Load]); the
// successfully-loaded skills are still returned, so callers may log and
// proceed.
func Discover(root string) (Result, error) {
	skills, err := Load(root)
	var res Result
	for _, s := range skills {
		if s.NeedsShell() {
			res.Skipped = append(res.Skipped, s.Name)
			slog.Warn("skill: refused script-bearing skill (shell execution needs bash approval, not yet available)",
				"skill", s.Name, "path", s.Path, "allowed_tools", s.AllowedTools)
			continue
		}
		res.Tools = append(res.Tools, adaptTool(s))
		res.Loaded = append(res.Loaded, s.Name)
	}
	if err != nil {
		return res, fmt.Errorf("load skills: %w", err)
	}
	return res, nil
}
