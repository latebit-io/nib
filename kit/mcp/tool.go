package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// ToolNamePrefix starts every MCP tool's name, followed by the server
// and the server's own tool name, so no two servers can collide.
const ToolNamePrefix = "mcp__"

// maxToolName is the longest tool name every provider accepts.
const maxToolName = 64

// maxResultBytes caps one tool reply. Above bash's cap: servers return
// documents, not command output.
const maxResultBytes = 32 << 10

// maxArgBytes caps the argument payload of one call.
const maxArgBytes = 64 << 10

// caller is the part of [Client] a tool needs.
type caller interface {
	CallToolResult(ctx context.Context, name string, args map[string]any) (ToolResult, error)
}

// tool is one server tool as an [agent.Tool].
type tool struct {
	client caller
	remote string // the name the server knows the tool by
	def    llm.ToolDef
}

// ToolName returns the name the model sees for a server's tool.
// Characters a provider would reject are replaced.
func ToolName(server, name string) string {
	return ToolNamePrefix + sanitize(server) + "__" + sanitize(name)
}

// sanitize keeps the characters every provider allows in a tool name.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}

// newTool adapts info, a tool of the named server. It fails when the
// resulting name is longer than providers accept.
func newTool(client caller, server string, info ToolInfo) (agent.Tool, error) {
	name := ToolName(server, info.Name)
	if len(name) > maxToolName {
		return nil, fmt.Errorf("tool name %s is over %d characters", name, maxToolName)
	}
	return &tool{client: client, remote: info.Name, def: llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        name,
			Description: info.Description,
			Parameters:  parameters(name, info.InputSchema),
		},
	}}, nil
}

// parameters converts a server's input schema. One the typed form
// cannot hold becomes an open object: the tool stays callable and the
// server validates the arguments itself.
func parameters(name string, schema json.RawMessage) llm.FunctionParams {
	open := llm.FunctionParams{Type: "object"}
	if len(schema) == 0 {
		return open
	}
	var params llm.FunctionParams
	err := json.Unmarshal(schema, &params)
	if err == nil && !representable(llm.FunctionParam{Type: "object", Properties: params.Properties}) {
		err = errors.New("a property has no single type, or an array has no items")
	}
	if err != nil {
		slog.Warn("mcp: input schema not representable, sending an open object", "tool", name, "err", err)
		return open
	}
	params.Type = "object"
	return params
}

// representable reports whether p and everything under it decoded
// with a type. A keyword the typed form lacks (anyOf, $ref) leaves the
// type empty, and a provider rejects the whole request over that.
func representable(p llm.FunctionParam) bool {
	if p.Type == "" || (p.Type == "array" && p.Items == nil) {
		return false
	}
	if p.Items != nil && !representable(*p.Items) {
		return false
	}
	for _, child := range p.Properties {
		if !representable(child) {
			return false
		}
	}
	return true
}

// Definition returns the tool's schema for the model.
func (t *tool) Definition() llm.ToolDef { return t.def }

// Execute forwards the call to the server. A failure the server reports
// comes back as an error result the model can react to.
func (t *tool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	raw := call.Function.Arguments
	if len(raw) > maxArgBytes {
		return agent.ToolResult{Content: "Error: arguments too large", IsError: true}
	}
	args := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return agent.ToolResult{Content: fmt.Sprintf("Error: invalid arguments: %v", err), IsError: true}
		}
	}
	result, err := t.client.CallToolResult(ctx, t.remote, args)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}
	}
	text := result.Text
	if len(text) > maxResultBytes {
		// Cut on a rune boundary; a split rune would garble the tail.
		text = strings.ToValidUTF8(text[:maxResultBytes], "") + "\n[... output truncated]"
	}
	return agent.ToolResult{Content: text, IsError: result.IsError}
}
