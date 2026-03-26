// Package mcp implements a Model Context Protocol client over stdio.
// It speaks JSON-RPC 2.0 to an MCP server subprocess, supporting
// tool discovery and invocation.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// ToolInfo describes a tool exposed by an MCP server.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client communicates with an MCP server over stdio using JSON-RPC 2.0.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader

	mu     sync.Mutex
	nextID int

	// pending tracks in-flight requests awaiting responses.
	pending map[int]chan json.RawMessage
}

// jsonRPCRequest is the wire format for a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is the wire format for a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError is the error object in a JSON-RPC 2.0 response.
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NewStdioClient spawns an MCP server subprocess and connects via stdio.
// The command is executed with the given arguments and environment.
func NewStdioClient(command string, args []string, env []string) (*Client, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %q: %w", command, err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReaderSize(stdout, 64*1024),
		pending: make(map[int]chan json.RawMessage),
	}

	go c.readLoop()

	return c, nil
}

// maxLineSize caps each JSON-RPC message from the MCP server (10MB, same as SSE scanner).
const maxLineSize = 10 * 1024 * 1024

// readLoop reads JSON-RPC responses from stdout and dispatches to pending requests.
func (c *Client) readLoop() {
	scanner := bufio.NewScanner(c.reader)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()

		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			slog.Debug("mcp: ignoring non-JSON line", "line", string(line))
			continue
		}

		// Notifications (no ID) are ignored for now.
		if resp.ID == nil {
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[*resp.ID]
		if ok {
			delete(c.pending, *resp.ID)
		}
		c.mu.Unlock()

		if ok {
			if resp.Error != nil {
				// Marshal cannot fail: jsonRPCError contains only int and string.
				errJSON, _ := json.Marshal(resp.Error)
				ch <- errJSON
			} else {
				ch <- resp.Result
			}
			close(ch)
		}
	}

	// Scanner stopped — EOF or error. Clean up pending requests.
	err := scanner.Err()
	if err != nil {
		slog.Debug("mcp: read loop error", "err", err)
	} else {
		slog.Debug("mcp: read loop ended", "err", "EOF")
	}
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// call sends a JSON-RPC request and waits for the response.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}
	data = append(data, '\n')

	c.mu.Lock()
	_, err = c.stdin.Write(data)
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("mcp: write request: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case result, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp: connection closed")
		}
		// Check if result is actually an error response.
		var rpcErr jsonRPCError
		if json.Unmarshal(result, &rpcErr) == nil && rpcErr.Code != 0 {
			return nil, fmt.Errorf("mcp: server error %d: %s", rpcErr.Code, rpcErr.Message)
		}
		return result, nil
	}
}

// Initialize performs the MCP protocol handshake.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "junto",
			"version": "0.1.0",
		},
	}
	_, err := c.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}

	// Send initialized notification (no response expected).
	notif := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	// Marshal cannot fail: notification contains only string fields.
	data, _ := json.Marshal(notif)
	data = append(data, '\n')
	c.mu.Lock()
	_, err = c.stdin.Write(data)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("mcp: send initialized: %w", err)
	}

	return nil
}

// ListTools discovers the tools available on the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal tools: %w", err)
	}
	return resp.Tools, nil
}

// CallTool invokes a tool on the MCP server and returns the text result.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	result, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return "", err
	}

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("mcp: unmarshal tool result: %w", err)
	}

	// Concatenate all text content blocks.
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// Close terminates the MCP server subprocess.
func (c *Client) Close() error {
	if err := c.stdin.Close(); err != nil {
		slog.Debug("mcp: close stdin", "err", err)
	}
	return c.cmd.Wait()
}
