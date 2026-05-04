package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockMCPServer is a shell script that acts as a minimal MCP server.
// It reads JSON-RPC requests from stdin and writes responses to stdout.
const mockMCPServer = `#!/bin/sh
while IFS= read -r line; do
  id=$(echo "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(echo "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')

  case "$method" in
    "initialize")
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"serverInfo\":{\"name\":\"mock\",\"version\":\"0.1.0\"}}}"
      ;;
    "tools/list")
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"tools\":[{\"name\":\"mark_fetch\",\"description\":\"Fetch a doc\",\"inputSchema\":{\"type\":\"object\",\"properties\":{\"url\":{\"type\":\"string\",\"description\":\"path\"}},\"required\":[\"url\"]}}]}}"
      ;;
    "tools/call")
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"hello from mock\"}]}}"
      ;;
    *)
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"error\":{\"code\":-32601,\"message\":\"method not found\"}}"
      ;;
  esac
done
`

func writeMockServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mock_mcp.sh")
	if err := os.WriteFile(path, []byte(mockMCPServer), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClient_Initialize(t *testing.T) {
	script := writeMockServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewStdioClient("sh", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }() // test cleanup — error irrelevant

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
}

func TestClient_ListTools(t *testing.T) {
	script := writeMockServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewStdioClient("sh", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }() // test cleanup — error irrelevant

	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "mark_fetch" {
		t.Errorf("expected tool name 'mark_fetch', got %q", tools[0].Name)
	}
	if tools[0].Description != "Fetch a doc" {
		t.Errorf("expected description 'Fetch a doc', got %q", tools[0].Description)
	}

	// Verify schema was captured
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("failed to parse input schema: %v", err)
	}
	if _, ok := schema.Properties["url"]; !ok {
		t.Error("expected 'url' in input schema properties")
	}
}

func TestClient_CallTool(t *testing.T) {
	script := writeMockServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewStdioClient("sh", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }() // test cleanup — error irrelevant

	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	result, err := client.CallTool(ctx, "mark_fetch", map[string]any{"url": "/index.md"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if !strings.Contains(result, "hello from mock") {
		t.Errorf("expected 'hello from mock' in result, got %q", result)
	}
}

func TestClient_ContextCancellation(t *testing.T) {
	script := writeMockServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	client, err := NewStdioClient("sh", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }() // test cleanup — error irrelevant

	err = client.Initialize(ctx)
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}

func TestClient_CallAfterCloseLeavesPendingEmpty(t *testing.T) {
	// Guards against the TOCTOU race where closed/done is checked before
	// mu, then readLoop purges pending between the check and the enqueue,
	// leaving an orphaned pending entry nobody will ever clean up. After
	// N post-close calls the pending map must still be empty.
	script := writeMockServer(t)
	client, err := NewStdioClient("sh", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i := range 100 {
		_, err := client.CallTool(ctx, "mark_fetch", map[string]any{"url": "/x"})
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("call %d: expected ErrClientClosed, got: %v", i, err)
		}
	}

	client.mu.Lock()
	n := len(client.pending)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("pending must be empty after post-close calls, got %d entries", n)
	}
}

func TestClient_AfterCloseFailsFast(t *testing.T) {
	// After Close, further calls must return ErrClientClosed rather than
	// hanging or falling through to EPIPE / ctx-deadline errors. The
	// sentinel alone establishes this: without the guard, call() would
	// surface "mcp: write request: ..." (EPIPE on a torn-down pipe) or
	// ctx.DeadlineExceeded — never ErrClientClosed. A wall-clock bound
	// is deliberately avoided; timing assertions flake under CI jitter.
	script := writeMockServer(t)
	client, err := NewStdioClient("sh", []string{script}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = client.CallTool(ctx, "mark_fetch", map[string]any{"url": "/x"})
	if err == nil {
		t.Fatal("expected error after Close")
	}
	if !errors.Is(err, ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got: %v", err)
	}
}
