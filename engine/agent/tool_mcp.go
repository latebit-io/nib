package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/mcp"
)

// MCPToolAdapter wraps a single MCP server tool as an agent.Tool.
// The agent loop sees it as any other tool — it doesn't know about MCP.
type MCPToolAdapter struct {
	client  *mcp.Client
	info    mcp.ToolInfo
	toolDef llm.ToolDef
}

// NewMCPToolAdapter creates an adapter for one MCP tool.
// It converts the MCP tool's JSON Schema into an OpenAI-compatible ToolDef.
func NewMCPToolAdapter(client *mcp.Client, info mcp.ToolInfo) *MCPToolAdapter {
	toolDef := llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        info.Name,
			Description: info.Description,
			Parameters:  convertSchema(info.InputSchema),
		},
	}
	return &MCPToolAdapter{
		client:  client,
		info:    info,
		toolDef: toolDef,
	}
}

// Definition returns the OpenAI-compatible tool schema.
func (t *MCPToolAdapter) Definition() llm.ToolDef {
	return t.toolDef
}

// Execute calls the MCP tool and returns the result string.
func (t *MCPToolAdapter) Execute(ctx context.Context, call llm.ToolCall) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}

	slog.Debug("mcp tool call", "tool", t.info.Name, "args", args)

	result, err := t.client.CallTool(ctx, t.info.Name, args)
	if err != nil {
		slog.Error("mcp tool error", "tool", t.info.Name, "err", err)
		return fmt.Sprintf("Error: %v", err)
	}
	return result
}

// convertSchema converts an MCP JSON Schema (raw JSON) into the
// OpenAI-compatible FunctionParams used by our tool definitions.
func convertSchema(schema json.RawMessage) llm.FunctionParams {
	if len(schema) == 0 {
		return llm.FunctionParams{
			Type:       "object",
			Properties: map[string]llm.FunctionParam{},
		}
	}

	var raw struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &raw); err != nil {
		slog.Warn("mcp: failed to parse tool schema", "err", err)
		return llm.FunctionParams{
			Type:       "object",
			Properties: map[string]llm.FunctionParam{},
		}
	}

	props := make(map[string]llm.FunctionParam, len(raw.Properties))
	for name, p := range raw.Properties {
		props[name] = llm.FunctionParam{
			Type:        p.Type,
			Description: p.Description,
		}
	}

	return llm.FunctionParams{
		Type:       "object",
		Properties: props,
		Required:   raw.Required,
	}
}
