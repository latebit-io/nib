package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
)

// maxMCPResult caps the response size returned to the LLM from MCP tools.
// Higher than BashTool's 8KB cap because MCP tools return markdown documents
// (architecture specs, patterns, roadmaps) that are legitimately larger than
// typical bash output (build errors, test results).
const maxMCPResult = 32 * 1024

// maxMCPArgs caps the argument payload size from the LLM to prevent
// excessive allocation from malformed tool calls.
const maxMCPArgs = 64 * 1024

// MCPCaller is the interface the adapter needs from an MCP client.
// Defined here so the agent package has no dependency on engine/mcp.
type MCPCaller interface {
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// MCPToolInfo describes an MCP tool's schema. This is the agent package's
// own representation — the caller maps from the transport-specific type
// (e.g. mcp.ToolInfo) during wiring.
type MCPToolInfo struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// MCPToolAdapter wraps a single MCP server tool as an agent.Tool.
// The agent loop sees it as any other tool — it doesn't know about MCP.
//
// Trust model: MCP servers are developer-configured (via .mcp.json).
// Their responses are treated as trusted content — no sanitization is
// applied, same as file content from read_file. Size is capped to
// prevent token blow-up from unexpectedly large responses.
type MCPToolAdapter struct {
	client   MCPCaller
	toolName string
	toolDef  llm.ToolDef
}

// NewMCPToolAdapter creates an adapter for one MCP tool.
// It converts the tool's JSON Schema into an OpenAI-compatible ToolDef.
func NewMCPToolAdapter(client MCPCaller, info MCPToolInfo) *MCPToolAdapter {
	toolDef := llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        info.Name,
			Description: info.Description,
			Parameters:  convertSchema(info.InputSchema),
		},
	}
	return &MCPToolAdapter{
		client:   client,
		toolName: info.Name,
		toolDef:  toolDef,
	}
}

// Definition returns the OpenAI-compatible tool schema.
func (t *MCPToolAdapter) Definition() llm.ToolDef {
	return t.toolDef
}

// Execute calls the MCP tool and returns the result string.
func (t *MCPToolAdapter) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if len(call.Function.Arguments) > maxMCPArgs {
		return textResult("Error: arguments too large")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}

	slog.Debug("mcp tool call", "tool", t.toolName, "args", args)

	result, err := t.client.CallTool(ctx, t.toolName, args)
	if err != nil {
		slog.Error("mcp tool error", "tool", t.toolName, "err", err)
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	if len(result) > maxMCPResult {
		// Truncate at valid UTF-8 boundary to avoid garbled output.
		result = strings.ToValidUTF8(result[:maxMCPResult], "") + "\n[... output truncated]"
	}
	return textResult(result)
}

// convertSchema converts a JSON Schema (raw JSON) into the OpenAI-compatible
// FunctionParams used by our tool definitions.
//
// Limitation: only flat schemas are supported — top-level properties with
// scalar types (string, integer, boolean, number). Nested objects, arrays,
// anyOf, $ref, and other rich JSON Schema features are silently dropped.
// Properties with unsupported types will have empty type/description fields
// in the resulting ToolDef.
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
