package agent

import (
	"encoding/json"
	"testing"
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
