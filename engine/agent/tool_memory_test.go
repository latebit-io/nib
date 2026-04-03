package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/memory"
)

// mockStore implements memory.Store for testing.
type mockStore struct {
	fetchDoc memory.Document
	fetchErr error

	publishDoc memory.Document
	publishErr error

	appendDoc memory.Document
	appendErr error

	listPaths []string
	listErr   error

	// Track last calls for assertions.
	lastFetchPath   string
	lastPublishPath string
	lastPublishBody string
	lastPublishVer  int
	lastAppendPath  string
	lastAppendBody  string
	lastAppendVer   int
	lastListPath    string
}

func (m *mockStore) Fetch(_ context.Context, path string) (memory.Document, error) {
	m.lastFetchPath = path
	return m.fetchDoc, m.fetchErr
}

func (m *mockStore) Publish(_ context.Context, path, body string, expectedVersion int) (memory.Document, error) {
	m.lastPublishPath = path
	m.lastPublishBody = body
	m.lastPublishVer = expectedVersion
	return m.publishDoc, m.publishErr
}

func (m *mockStore) Append(_ context.Context, path, body string, expectedVersion int) (memory.Document, error) {
	m.lastAppendPath = path
	m.lastAppendBody = body
	m.lastAppendVer = expectedVersion
	return m.appendDoc, m.appendErr
}

func (m *mockStore) List(_ context.Context, path string) ([]string, error) {
	m.lastListPath = path
	return m.listPaths, m.listErr
}

func toolCall(name, args string) llm.ToolCall {
	return llm.ToolCall{
		ID: "test-id",
		Function: llm.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}
}

func TestMemoryFetchTool(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		store      mockStore
		wantSubstr string
	}{
		{
			name: "success",
			args: `{"path": "/index.md"}`,
			store: mockStore{
				fetchDoc: memory.Document{
					Path: "/index.md", Body: "# Hello", Version: 3, Modified: "2026-04-01T00:00:00Z",
				},
			},
			wantSubstr: "version=3",
		},
		{
			name:       "missing path",
			args:       `{}`,
			wantSubstr: "Error: path is required",
		},
		{
			name:       "invalid json",
			args:       `{bad`,
			wantSubstr: "Error: invalid arguments",
		},
		{
			name:       "not found",
			args:       `{"path": "/nope.md"}`,
			store:      mockStore{fetchErr: memory.ErrNotFound},
			wantSubstr: "Error: memory: document not found",
		},
		{
			name: "section extraction",
			args: `{"path": "/doc.md", "section": "Current State"}`,
			store: mockStore{
				fetchDoc: memory.Document{
					Path: "/doc.md", Version: 2, Modified: "2026-04-01T00:00:00Z",
					Body: "# Project\n\n## Current State\n\nBuilding memory.\n\n## Next Steps\n\nShip it.\n",
				},
			},
			wantSubstr: "Building memory.",
		},
		{
			name: "section not found lists available",
			args: `{"path": "/doc.md", "section": "Nonexistent"}`,
			store: mockStore{
				fetchDoc: memory.Document{
					Path: "/doc.md", Version: 1, Modified: "2026-04-01T00:00:00Z",
					Body: "# Project\n\n## Alpha\n\nContent.\n\n## Beta\n\nMore.\n",
				},
			},
			wantSubstr: "- Alpha",
		},
		{
			name: "section case insensitive",
			args: `{"path": "/doc.md", "section": "current state"}`,
			store: mockStore{
				fetchDoc: memory.Document{
					Path: "/doc.md", Version: 1, Modified: "2026-04-01T00:00:00Z",
					Body: "## Current State\n\nFound it.\n",
				},
			},
			wantSubstr: "Found it.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := NewMemoryFetchTool(&tt.store)
			result := tool.Execute(context.Background(), toolCall("memory_fetch", tt.args))
			if !strings.Contains(result.Content, tt.wantSubstr) {
				t.Errorf("got %q, want substring %q", result.Content, tt.wantSubstr)
			}
		})
	}
}

func TestMemoryPublishTool(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		store      mockStore
		wantSubstr string
	}{
		{
			name: "create success",
			args: `{"path": "/arch.md", "body": "# Architecture", "expected_version": 0}`,
			store: mockStore{
				publishDoc: memory.Document{Path: "/arch.md", Version: 1},
			},
			wantSubstr: "Published /arch.md (version=1)",
		},
		{
			name: "update success",
			args: `{"path": "/arch.md", "body": "# Updated", "expected_version": 2}`,
			store: mockStore{
				publishDoc: memory.Document{Path: "/arch.md", Version: 3},
			},
			wantSubstr: "Published /arch.md (version=3)",
		},
		{
			name:       "missing path",
			args:       `{"body": "content", "expected_version": 0}`,
			wantSubstr: "Error: path is required",
		},
		{
			name:       "missing body",
			args:       `{"path": "/x.md", "expected_version": 0}`,
			wantSubstr: "Error: body is required",
		},
		{
			name:       "negative version rejected",
			args:       `{"path": "/x.md", "body": "content", "expected_version": -1}`,
			wantSubstr: "Error: expected_version must be >= 0",
		},
		{
			name:       "conflict error",
			args:       `{"path": "/x.md", "body": "content", "expected_version": 1}`,
			store:      mockStore{publishErr: memory.ErrConflict},
			wantSubstr: "Error: memory: version conflict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := NewMemoryPublishTool(&tt.store)
			result := tool.Execute(context.Background(), toolCall("memory_publish", tt.args))
			if !strings.Contains(result.Content, tt.wantSubstr) {
				t.Errorf("got %q, want substring %q", result.Content, tt.wantSubstr)
			}
		})
	}
}

func TestMemoryAppendTool(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		store      mockStore
		wantSubstr string
	}{
		{
			name: "success",
			args: `{"path": "/journal.md", "body": "## Entry", "expected_version": 2}`,
			store: mockStore{
				appendDoc: memory.Document{Path: "/journal.md", Version: 3},
			},
			wantSubstr: "Appended to /journal.md (version=3)",
		},
		{
			name:       "version 0 rejected",
			args:       `{"path": "/j.md", "body": "text", "expected_version": 0}`,
			wantSubstr: "Error: expected_version must be >= 1",
		},
		{
			name:       "missing path",
			args:       `{"body": "text", "expected_version": 1}`,
			wantSubstr: "Error: path is required",
		},
		{
			name:       "missing body",
			args:       `{"path": "/j.md", "expected_version": 1}`,
			wantSubstr: "Error: body is required",
		},
		{
			name:       "store error",
			args:       `{"path": "/j.md", "body": "text", "expected_version": 1}`,
			store:      mockStore{appendErr: errors.New("boom")},
			wantSubstr: "Error: boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := NewMemoryAppendTool(&tt.store)
			result := tool.Execute(context.Background(), toolCall("memory_append", tt.args))
			if !strings.Contains(result.Content, tt.wantSubstr) {
				t.Errorf("got %q, want substring %q", result.Content, tt.wantSubstr)
			}
		})
	}
}

func TestMemoryListTool(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		store      mockStore
		wantSubstr string
	}{
		{
			name:       "success with results",
			args:       `{"path": "/"}`,
			store:      mockStore{listPaths: []string{"/index.md", "/journal.md", "/arch.md"}},
			wantSubstr: "/index.md\n/journal.md\n/arch.md",
		},
		{
			name:       "empty results",
			args:       `{"path": "/empty/"}`,
			store:      mockStore{listPaths: nil},
			wantSubstr: "No documents found.",
		},
		{
			name:       "missing path",
			args:       `{}`,
			wantSubstr: "Error: path is required",
		},
		{
			name:       "store error",
			args:       `{"path": "/"}`,
			store:      mockStore{listErr: memory.ErrServer},
			wantSubstr: "Error: memory: server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := NewMemoryListTool(&tt.store)
			result := tool.Execute(context.Background(), toolCall("memory_list", tt.args))
			if !strings.Contains(result.Content, tt.wantSubstr) {
				t.Errorf("got %q, want substring %q", result.Content, tt.wantSubstr)
			}
		})
	}
}

func TestMemoryToolsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &mockStore{}
	call := toolCall("test", `{"path": "/x.md"}`)

	tools := []Tool{
		NewMemoryFetchTool(store),
		NewMemoryPublishTool(store),
		NewMemoryAppendTool(store),
		NewMemoryListTool(store),
	}

	for _, tool := range tools {
		def := tool.Definition()
		t.Run(def.Function.Name, func(t *testing.T) {
			result := tool.Execute(ctx, call)
			if !strings.Contains(result.Content, "Error: agent canceled") {
				t.Errorf("got %q, want agent canceled", result.Content)
			}
		})
	}
}

func TestExtractSection(t *testing.T) {
	doc := `# Project

## Current State

Building memory integration.
All tests pass.

## Next Steps

Ship it.
Release v1.

## Done

Nothing yet.
`

	tests := []struct {
		name       string
		section    string
		wantFound  bool
		wantSubstr string
		wantAbsent string
	}{
		{
			name:       "exact match",
			section:    "Current State",
			wantFound:  true,
			wantSubstr: "Building memory",
			wantAbsent: "Ship it",
		},
		{
			name:       "case insensitive",
			section:    "next steps",
			wantFound:  true,
			wantSubstr: "Release v1",
			wantAbsent: "Building memory",
		},
		{
			name:       "last section",
			section:    "Done",
			wantFound:  true,
			wantSubstr: "Nothing yet",
		},
		{
			name:      "not found",
			section:   "Nonexistent",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			section, ok := extractSection(doc, tt.section)
			if ok != tt.wantFound {
				t.Fatalf("found=%v, want %v", ok, tt.wantFound)
			}
			if !ok {
				return
			}
			if tt.wantSubstr != "" && !strings.Contains(section, tt.wantSubstr) {
				t.Errorf("section should contain %q, got:\n%s", tt.wantSubstr, section)
			}
			if tt.wantAbsent != "" && strings.Contains(section, tt.wantAbsent) {
				t.Errorf("section should not contain %q, got:\n%s", tt.wantAbsent, section)
			}
		})
	}
}

func TestListSections(t *testing.T) {
	doc := "# Title\n\n## Alpha\n\nContent.\n\n## Beta\n\nMore.\n"
	result := listSections(doc)
	if !strings.Contains(result, "- Alpha") {
		t.Errorf("expected Alpha in sections, got:\n%s", result)
	}
	if !strings.Contains(result, "- Beta") {
		t.Errorf("expected Beta in sections, got:\n%s", result)
	}

	empty := listSections("No headings here.")
	if empty != "(no sections found)" {
		t.Errorf("expected no sections message, got: %q", empty)
	}
}

func TestMemoryToolDefinitions(t *testing.T) {
	store := &mockStore{}
	tools := []struct {
		name string
		tool Tool
	}{
		{"memory_fetch", NewMemoryFetchTool(store)},
		{"memory_publish", NewMemoryPublishTool(store)},
		{"memory_append", NewMemoryAppendTool(store)},
		{"memory_list", NewMemoryListTool(store)},
	}

	for _, tt := range tools {
		t.Run(tt.name, func(t *testing.T) {
			def := tt.tool.Definition()
			if def.Function.Name != tt.name {
				t.Errorf("name: got %q, want %q", def.Function.Name, tt.name)
			}
			if def.Type != "function" {
				t.Errorf("type: got %q, want %q", def.Type, "function")
			}
			if def.Function.Description == "" {
				t.Error("description is empty")
			}
			if len(def.Function.Parameters.Required) == 0 {
				t.Error("no required parameters")
			}
		})
	}
}
