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

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/latebit-io/nib/kit/elicit"
)

// mockMCPServer is a shell script that acts as a server of the
// 2024-11-05 revision: it knows initialize, tools/list and tools/call,
// and refuses everything else, server/discover included.
const mockMCPServer = `#!/bin/sh
while IFS= read -r line; do
  id=$(echo "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(echo "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  [ -z "$id" ] && continue

  case "$method" in
    "initialize")
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"mock\",\"version\":\"0.1.0\"}}}"
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
	path := filepath.Join(t.TempDir(), "mock_mcp.sh")
	if err := os.WriteFile(path, []byte(mockMCPServer), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// startMock connects to the shell server.
func startMock(t *testing.T) *Client {
	t.Helper()
	client, err := NewStdioClient("sh", []string{writeMockServer(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() }) // test cleanup; the assertions are the test
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return client
}

// TestClient_EarlierRevision: a server that refuses server/discover
// gets the handshake of its own revision.
func TestClient_EarlierRevision(t *testing.T) {
	client := startMock(t)
	if info := client.Server(); info.Protocol != "2024-11-05" || !info.Tools {
		t.Errorf("Server() = %+v, want the mock on 2024-11-05 with tools", info)
	}
	ctx := context.Background()
	tools, err := client.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "mark_fetch" || tools[0].Description != "Fetch a doc" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil || schema.Properties["url"] == nil {
		t.Errorf("schema = %s, %v; want the url property", tools[0].InputSchema, err)
	}
	result, err := client.CallTool(ctx, "mark_fetch", map[string]any{"url": "/index.md"})
	if err != nil || result != "hello from mock" {
		t.Errorf("CallTool = %q, %v", result, err)
	}
}

func TestClient_CancelledInitialize(t *testing.T) {
	client, err := NewStdioClient("sh", []string{writeMockServer(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }() // test cleanup; the assertion is the test
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Initialize(ctx); err == nil {
		t.Error("Initialize on a cancelled context must fail")
	}
}

func TestClient_MissingCommand(t *testing.T) {
	if _, err := NewStdioClient(filepath.Join(t.TempDir(), "no-such-binary"), nil, nil); err == nil {
		t.Error("a command that does not exist must fail before anything starts")
	}
}

// TestClient_AfterCloseFailsFast: calls after Close return the
// sentinel instead of hanging on a torn-down connection.
func TestClient_AfterCloseFailsFast(t *testing.T) {
	client := startMock(t)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if _, err := client.CallTool(context.Background(), "mark_fetch", nil); !errors.Is(err, ErrClientClosed) {
		t.Errorf("CallTool after Close = %v, want ErrClientClosed", err)
	}
	if err := client.Initialize(context.Background()); !errors.Is(err, ErrClientClosed) {
		t.Errorf("Initialize after Close = %v, want ErrClientClosed", err)
	}
}

// inMemory connects a Client to server through the SDK's in-process
// transport, which speaks the newest revision.
func inMemory(t *testing.T, server *sdk.Server) *Client {
	t.Helper()
	serverSide, clientSide := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ss, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() }) // test cleanup
	client := &Client{transport: func() sdk.Transport { return clientSide }}
	t.Cleanup(func() { _ = client.Close() }) // test cleanup
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return client
}

// testServer offers one of everything: a tool, a prompt, a resource.
func testServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{Instructions: "Use echo to echo."})
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "Echoes"}, func(_ context.Context, _ *sdk.CallToolRequest, in struct {
		Text string `json:"text"`
	}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: in.Text}, &sdk.ImageContent{MIMEType: "image/png", Data: []byte("png")}}}, nil, nil
	})
	server.AddPrompt(&sdk.Prompt{Name: "greet", Arguments: []*sdk.PromptArgument{{Name: "who", Required: true}}},
		func(_ context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{
				{Role: "user", Content: &sdk.TextContent{Text: "Greet " + req.Params.Arguments["who"]}},
				{Role: "assistant", Content: &sdk.TextContent{Text: "Hello"}},
			}}, nil
		})
	server.AddResource(&sdk.Resource{URI: "file:///notes.txt", Name: "notes", MIMEType: "text/plain"},
		func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "remember the milk"}}}, nil
		})
	return server
}

// TestClient_NewestRevision: through the newest revision, the client
// reaches tools, prompts and resources.
func TestClient_NewestRevision(t *testing.T) {
	client := inMemory(t, testServer())
	info := client.Server()
	if info.Protocol != "2026-07-28" || info.Instructions != "Use echo to echo." || !info.Tools || !info.Prompts || !info.Resources {
		t.Errorf("Server() = %+v, want 2026-07-28 with instructions and all three features", info)
	}
	ctx := context.Background()
	got, err := client.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil || got != "hi\n[image, image/png, 3 bytes, not shown]" {
		t.Errorf("CallTool = %q, %v; want the text and the image named", got, err)
	}
	prompts, err := client.prompts(ctx)
	if err != nil || len(prompts) != 1 || prompts[0].name != "greet" || !prompts[0].arguments[0].required {
		t.Fatalf("prompts = %+v, %v", prompts, err)
	}
	text, err := client.getPrompt(ctx, "greet", map[string]string{"who": "Ada"})
	if err != nil || text != "Greet Ada\n\nAssistant: Hello" {
		t.Errorf("getPrompt = %q, %v", text, err)
	}
	resources, err := client.resources(ctx, 0)
	if err != nil || len(resources) != 1 || resources[0].address != "file:///notes.txt" {
		t.Fatalf("resources = %+v, %v", resources, err)
	}
	if text, err := client.readResource(ctx, "file:///notes.txt"); err != nil || text != "remember the milk" {
		t.Errorf("readResource = %q, %v", text, err)
	}
}

// askServer has a tool that asks the person for a name before it answers.
func askServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "asker", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "hello", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			answer, ok := req.Params.InputResponses["name"].(*sdk.ElicitResult)
			if !ok {
				return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"name": &sdk.ElicitParams{
					Mode: "form", Message: "Who are you?",
					RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}},
				}}}, nil
			}
			if answer.Action != elicit.Accept {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "no name: " + answer.Action}}}, nil
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hello " + answer.Content["name"].(string)}}}, nil
		})
	return server
}

// TestClient_Elicitation: a question the server asks in the middle of a
// call reaches the elicitor, and the answer reaches the server. With
// nobody to ask, the question is dismissed.
func TestClient_Elicitation(t *testing.T) {
	client := inMemory(t, askServer())
	ctx := context.Background()
	if got, err := client.CallTool(ctx, "hello", nil); err != nil || got != "no name: cancel" {
		t.Errorf("without an elicitor: %q, %v; want the question dismissed", got, err)
	}
	var asked elicit.Request
	client.SetAsker(func(_ context.Context, r elicit.Request) (elicit.Result, error) {
		asked = r
		return elicit.Result{Action: elicit.Accept, Content: map[string]any{"name": "Ada"}}, nil
	})
	if got, err := client.CallTool(ctx, "hello", nil); err != nil || got != "hello Ada" {
		t.Errorf("with an elicitor: %q, %v", got, err)
	}
	if asked.Mode != elicit.Form || asked.Message != "Who are you?" || len(asked.Fields) != 1 || !asked.Fields[0].Required {
		t.Errorf("asked %+v, want the form with its one required field", asked)
	}
}

// TestClient_ListChanged: a tool added while connected is announced.
func TestClient_ListChanged(t *testing.T) {
	server := testServer()
	client := inMemory(t, server)
	changes := make(chan Change, 4)
	client.Watch(func(c Change) { changes <- c })
	server.AddTool(&sdk.Tool{Name: "later", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	select {
	case c := <-changes:
		if c != ToolsChanged {
			t.Errorf("change = %v, want ToolsChanged", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no change announced")
	}
	tools, err := client.ListTools(context.Background())
	if err != nil || !strings.Contains(toolNames(tools), "later") {
		t.Errorf("tools after the change: %v, %v", toolNames(tools), err)
	}
}

func toolNames(tools []ToolInfo) string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return strings.Join(names, ",")
}

// TestRequest_PageMode: a page to open must be a web address.
func TestRequest_PageMode(t *testing.T) {
	r, err := request(&sdk.ElicitParams{Mode: elicit.Page, Message: "Connect your account", URL: "https://auth.example/start?x=1"})
	if err != nil || r.URL != "https://auth.example/start?x=1" || r.Mode != elicit.Page {
		t.Errorf("request = %+v, %v", r, err)
	}
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://"} {
		if _, err := request(&sdk.ElicitParams{Mode: elicit.Page, URL: bad}); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// exitOnDiscover is a server of an earlier revision that exits when it
// sees a request it does not know, as some do.
const exitOnDiscover = `#!/bin/sh
while IFS= read -r line; do
  id=$(echo "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(echo "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  [ -z "$id" ] && continue
  case "$method" in
    "server/discover") exit 1 ;;
    "initialize")
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"serverInfo\":{\"name\":\"old\",\"version\":\"1\"}}}"
      ;;
  esac
done
`

// TestClient_ServerExitsOnDiscover: such a server is started again
// and gets the earlier handshake.
func TestClient_ServerExitsOnDiscover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sh")
	if err := os.WriteFile(path, []byte(exitOnDiscover), 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := NewStdioClient("sh", []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() }) // test cleanup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := client.Server().Protocol; got != "2025-06-18" {
		t.Errorf("protocol = %q, want the earlier handshake", got)
	}
}

// TestClient_ChangesBeforeWatch: a change announced before anyone
// watches is passed on once someone does.
func TestClient_ChangesBeforeWatch(t *testing.T) {
	c := &Client{}
	c.changed(ToolsChanged)
	c.changed(ToolsChanged)
	var got []Change
	c.Watch(func(ch Change) { got = append(got, ch) })
	if len(got) != 1 || got[0] != ToolsChanged {
		t.Errorf("replayed %v, want one ToolsChanged", got)
	}
}

// hangOnCall answers the handshake, then never answers a tool call.
const hangOnCall = `#!/bin/sh
while IFS= read -r line; do
  id=$(echo "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(echo "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  [ -z "$id" ] && continue
  case "$method" in
    "initialize")
      echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"stuck\",\"version\":\"1\"}}}"
      ;;
    "tools/call") ;;
    *) echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"error\":{\"code\":-32601,\"message\":\"no\"}}" ;;
  esac
done
`

// TestClient_CloseDoesNotWaitOnAStuckCall: a call the server never
// answers cannot hold Close; the process is killed.
func TestClient_CloseDoesNotWaitOnAStuckCall(t *testing.T) {
	closeGrace = 200 * time.Millisecond
	t.Cleanup(func() { closeGrace = 5 * time.Second })
	path := filepath.Join(t.TempDir(), "stuck.sh")
	if err := os.WriteFile(path, []byte(hangOnCall), 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := NewStdioClient("sh", []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	called := make(chan error, 1)
	go func() {
		_, err := client.CallTool(context.Background(), "anything", nil) // no deadline: it waits for ever
		called <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the call reach the server
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		// The server was killed, so Close may report how it ended; only the wait matters.
		t.Logf("Close: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on the stuck call")
	}
	select {
	case err := <-called:
		if err == nil {
			t.Error("the stuck call must fail once the server is gone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck call never returned")
	}
}
