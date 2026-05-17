package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/tools/truncate"
)

func TestConvertSchema(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "The URL to fetch"},
			"format": {"type": "string", "description": "Output format"}
		},
		"required": ["url"]
	}`)

	params := convertSchema(schema)

	if params.Type != "object" {
		t.Errorf("expected type 'object', got %q", params.Type)
	}
	if len(params.Properties) != 2 {
		t.Errorf("expected 2 properties, got %d", len(params.Properties))
	}
	if p, ok := params.Properties["url"]; !ok {
		t.Error("missing 'url' property")
	} else if p.Type != "string" {
		t.Errorf("expected url type 'string', got %q", p.Type)
	}
	if len(params.Required) != 1 || params.Required[0] != "url" {
		t.Errorf("expected required=['url'], got %v", params.Required)
	}
}

func TestConvertSchema_Empty(t *testing.T) {
	params := convertSchema(nil)
	if params.Type != "object" {
		t.Errorf("expected type 'object', got %q", params.Type)
	}
	if len(params.Properties) != 0 {
		t.Errorf("expected 0 properties, got %d", len(params.Properties))
	}
}

func TestConvertSchema_Invalid(t *testing.T) {
	params := convertSchema(json.RawMessage(`{invalid`))
	if params.Type != "object" {
		t.Errorf("expected type 'object', got %q", params.Type)
	}
}

func TestNewMCPToolAdapter_Definition(t *testing.T) {
	info := MCPToolInfo{
		Name:        "mark_fetch",
		Description: "Fetch a document",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "bare path"}
			},
			"required": ["url"]
		}`),
	}

	// Pass nil client — we only test Definition(), not Execute().
	adapter := NewMCPToolAdapter(nil, info)
	def := adapter.Definition()

	if def.Function.Name != "mark_fetch" {
		t.Errorf("expected name 'mark_fetch', got %q", def.Function.Name)
	}
	if def.Function.Description != "Fetch a document" {
		t.Errorf("expected description 'Fetch a document', got %q", def.Function.Description)
	}
	if _, ok := def.Function.Parameters.Properties["url"]; !ok {
		t.Error("missing 'url' parameter")
	}
}

// fakeMCPCaller returns a fixed response so the adapter's truncation
// path can be exercised without spinning up an MCP server.
type fakeMCPCaller struct {
	response string
}

func (f *fakeMCPCaller) CallTool(_ context.Context, _ string, _ map[string]any) (string, error) {
	return f.response, nil
}

func TestMCPToolAdapter_TruncatesOverCap(t *testing.T) {
	big := strings.Repeat("x", 100*1024)
	caller := &fakeMCPCaller{response: big}
	info := MCPToolInfo{Name: "huge_doc"}
	adapter := NewMCPToolAdapter(caller, info)
	sink := &recordingStash{}
	adapter.SetStash(sink)

	result := adapter.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "huge_doc", Arguments: "{}"},
	})

	if len(result.Content) > truncate.DefaultMaxBytes+256 {
		t.Fatalf("MCP result %d bytes exceeds cap+marker budget", len(result.Content))
	}
	if !strings.Contains(result.Content, "[Truncated: showing") {
		t.Fatalf("missing truncation marker")
	}
	if !strings.Contains(result.Content, "Full output: .project/tooltmp/") {
		t.Fatalf("marker missing Full-output path")
	}
	if sink.calls != 1 {
		t.Fatalf("expected sink invoked once, got %d", sink.calls)
	}
	if sink.lastBody != big {
		t.Fatalf("sink received %d bytes, want %d", len(sink.lastBody), len(big))
	}
}

func TestMCPToolAdapter_PassesThroughUnderCap(t *testing.T) {
	small := "doc body that fits easily under the cap"
	adapter := NewMCPToolAdapter(&fakeMCPCaller{response: small}, MCPToolInfo{Name: "small_doc"})
	sink := &recordingStash{}
	adapter.SetStash(sink)

	result := adapter.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "small_doc", Arguments: "{}"},
	})
	if result.Content != small {
		t.Fatalf("expected passthrough, got %q", result.Content)
	}
	if sink.calls != 0 {
		t.Fatalf("expected sink untouched, got %d calls", sink.calls)
	}
}
