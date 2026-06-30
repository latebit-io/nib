package subagent

import (
	"context"
	"encoding/json"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/agentdef"
)

// ToolNamePrefix namespaces subagent tool names so a definition can never
// shadow a built-in tool (built-ins are registered first and win on
// collision), mirroring [skill.ToolNamePrefix].
const ToolNamePrefix = "agent_"

// spawnTool adapts one [agentdef.Definition] into an agent tool. Invoking
// it runs the definition as a child agent on the supplied task and
// returns the child's final summary.
type spawnTool struct {
	def      llm.ToolDef
	agentDef agentdef.Definition
	spawner  *Spawner
}

// Definition returns the advertised schema: a single required `task`
// string, described by the subagent's own description.
func (t spawnTool) Definition() llm.ToolDef { return t.def }

// Execute runs the subagent on the requested task and returns its result
// as the tool output. A failed run (or a spawn error) is returned with
// IsError set so the parent sees it as a tool failure.
func (t spawnTool) Execute(ctx context.Context, call llm.ToolCall) upagent.ToolResult {
	var args struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return upagent.ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}
	}
	if strings.TrimSpace(args.Task) == "" {
		return upagent.ToolResult{Content: "the 'task' argument is required", IsError: true}
	}

	res, err := t.spawner.Spawn(ctx, t.agentDef, args.Task)
	if err != nil {
		return upagent.ToolResult{Content: "subagent error: " + err.Error(), IsError: true}
	}
	content := strings.TrimSpace(res.Summary)
	if !res.Success {
		if len(res.Errors) > 0 {
			content += "\n\nerrors:\n" + strings.Join(res.Errors, "\n")
		}
		if content == "" {
			content = "subagent did not complete successfully"
		}
		return upagent.ToolResult{Content: content, IsError: true}
	}
	return upagent.ToolResult{Content: content}
}

// AdaptTool builds the agent tool for a subagent definition, bound to the
// spawner that will run it.
func AdaptTool(def agentdef.Definition, s *Spawner) upagent.Tool {
	return spawnTool{
		def: llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        ToolNamePrefix + def.Name,
				Description: def.Description,
				Parameters: llm.FunctionParams{
					Type: "object",
					Properties: map[string]llm.FunctionParam{
						"task": {
							Type:        "string",
							Description: "The task for the " + def.Name + " subagent to carry out.",
						},
					},
					Required: []string{"task"},
				},
			},
		},
		agentDef: def,
		spawner:  s,
	}
}
