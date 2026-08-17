// Package mcp implements a Model Context Protocol client over stdio.
// It speaks JSON-RPC 2.0 to an MCP server subprocess, supporting
// tool discovery and invocation.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/nib/ai/brand"
)

// ErrClientClosed is returned by [Client] methods after [Client.Close]
// has been called. Callers that hold a cached reference (e.g. memory
// adapters) can surface this immediately instead of waiting for the
// underlying RPC to time out.
var ErrClientClosed = errors.New("mcp: client closed")

// ToolInfo describes a tool exposed by an MCP server.
type ToolInfo struct {
	// Name is the tool identifier.
	Name string `json:"name"`
	// Description explains what the tool does.
	Description string `json:"description"`
	// InputSchema is the JSON Schema for the tool's parameters.
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is the outcome of a tool invocation. Text is the concatenated
// content of all text blocks from the MCP response; IsError reflects the
// server-side isError flag, which MCP servers set when a tool handler
// reports a semantic failure (distinct from a transport error).
type ToolResult struct {
	Text    string
	IsError bool
}

// Client communicates with an MCP server over stdio using JSON-RPC 2.0.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	done   chan struct{} // closed when readLoop exits

	mu     sync.Mutex
	nextID int

	// pending tracks in-flight requests awaiting responses. Each channel
	// carries a [pendingResult] so readLoop can tag whether the response
	// was a JSON-RPC result or error, rather than re-sniffing in call.
	pending map[int]chan pendingResult

	// closed is set by Close so subsequent RPC calls short-circuit with
	// [ErrClientClosed] instead of writing to a torn-down pipe.
	closed atomic.Bool
	// closeOnce makes Close idempotent: a second call returns the
	// first call's error instead of double-closing stdin / re-Waiting.
	closeOnce sync.Once
	closeErr  error
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

// pendingResult is the discriminated response delivered to a waiting
// [Client.call] via its pending channel. Exactly one arm is set:
// rpcErr non-nil means the server returned a JSON-RPC error object;
// otherwise result holds the success payload. Carrying the
// discriminator explicitly avoids re-sniffing a result for a
// top-level "code" field, which misclassifies a success payload that
// happens to contain a numeric code as an error (and an error with
// code 0 as success).
type pendingResult struct {
	result json.RawMessage
	rpcErr *jsonRPCError
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
		_ = stdin.Close()  // close error irrelevant — process never started
		_ = stdout.Close() // close error irrelevant — process never started
		return nil, fmt.Errorf("mcp: start %q: %w", command, err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReaderSize(stdout, 64*1024),
		done:    make(chan struct{}),
		pending: make(map[int]chan pendingResult),
	}

	go c.readLoop()

	return c, nil
}

// maxLineSize caps each JSON-RPC message from the MCP server (10MB, same as SSE scanner).
const maxLineSize = 10 * 1024 * 1024

// readLoop reads JSON-RPC responses from stdout and dispatches to pending requests.
func (c *Client) readLoop() {
	defer close(c.done)
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
				ch <- pendingResult{rpcErr: resp.Error}
			} else {
				ch <- pendingResult{result: resp.Result}
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
	// The closed/done check must be atomic with the pending insertion:
	// without the mutex, readLoop could purge between the check and the
	// insert, leaving an orphaned entry in the pending map that nothing
	// will ever clean up.
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, ErrClientClosed
	}
	// readLoop closed its done channel — the subprocess is gone (EOF /
	// error) but Close() hasn't run yet. Surface the failure fast rather
	// than enqueuing onto pending and waiting for ctx to expire.
	select {
	case <-c.done:
		c.mu.Unlock()
		return nil, ErrClientClosed
	default:
	}
	id := c.nextID
	c.nextID++
	ch := make(chan pendingResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	// purgePending removes the map entry when call() aborts before the
	// response would naturally consume it. Without this, every
	// marshal/write failure leaks the entry; the map would grow
	// monotonically across the client's lifetime.
	purgePending := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		purgePending()
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}
	data = append(data, '\n')

	c.mu.Lock()
	// readLoop may have exited between our enqueue and this write. If so,
	// pending was already purged (our entry was added after) and the pipe
	// is about to EPIPE anyway. Return fast and clean our entry.
	select {
	case <-c.done:
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ErrClientClosed
	default:
	}
	_, err = c.stdin.Write(data)
	if err != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("mcp: write request: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		// If readLoop already extracted ch before we deleted, it may still
		// send/close. The buffered channel absorbs the orphaned send; GC cleans up.
		return nil, ctx.Err()
	case res, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp: connection closed")
		}
		// readLoop tagged which arm this is — no re-sniffing.
		if res.rpcErr != nil {
			return nil, fmt.Errorf("mcp: server error %d: %s", res.rpcErr.Code, res.rpcErr.Message)
		}
		return res.result, nil
	}
}

// Initialize performs the MCP protocol handshake.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    brand.MCPServerName,
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
// The server-side isError flag is ignored — callers that need to distinguish
// a handler-reported failure from a successful response should use
// CallToolResult instead.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	r, err := c.CallToolResult(ctx, name, args)
	if err != nil {
		return "", err
	}
	return r.Text, nil
}

// CallToolResult invokes a tool and returns the text plus the isError flag.
// isError is true when the MCP server's tool handler reported a semantic
// failure (e.g. invalid arguments, downstream error) — the transport itself
// succeeded, so err is nil. Callers that map handler errors onto sentinel
// errors (like the memory adapter) should use this method.
func (c *Client) CallToolResult(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	result, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return ToolResult{}, err
	}

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return ToolResult{}, fmt.Errorf("mcp: unmarshal tool result: %w", err)
	}

	// Concatenate all text content blocks, capped at maxLineSize.
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			if sb.Len()+len(block.Text) > maxLineSize {
				sb.WriteString("\n[... result truncated]")
				break
			}
			sb.WriteString(block.Text)
		}
	}
	return ToolResult{Text: sb.String(), IsError: resp.IsError}, nil
}

// closeGrace bounds how long Close waits for the subprocess to exit on
// its own (after stdin is closed) before escalating to SIGKILL. Mirrors
// the server package's grace-then-SIGKILL teardown window.
const closeGrace = 5 * time.Second

// Close terminates the MCP server subprocess and waits for the
// readLoop goroutine to finish its pending-channel cleanup. After Close
// returns, further calls to [Client.CallTool], [Client.CallToolResult],
// [Client.ListTools], and [Client.Initialize] return [ErrClientClosed]
// immediately — callers that hold a cached reference (memory adapter,
// agent store) don't wait for a now-useless RPC to time out.
//
// Close is bounded: a wedged MCP server that does not exit on stdin
// close is SIGKILLed after [closeGrace] so Close cannot hang forever.
// This matters because server.Manager.Stop calls this first, so an
// unbounded wait here would stall every downstream teardown step.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.closeLocked() })
	return c.closeErr
}

// closeLocked performs the one-time teardown behind [Client.Close].
func (c *Client) closeLocked() error {
	// Record closed state first so concurrent RPC calls short-circuit
	// rather than enqueuing onto pending and then being orphaned.
	c.closed.Store(true)
	if err := c.stdin.Close(); err != nil {
		slog.Debug("mcp: close stdin", "err", err)
	}

	// readLoop must finish draining the StdoutPipe before cmd.Wait():
	// os/exec forbids Wait while pipe reads are outstanding. A healthy
	// server exits on stdin close, EOFs stdout, and readLoop closes
	// c.done promptly. A wedged server never exits, so bound the wait —
	// on expiry SIGKILL the process, which forces stdout EOF and unblocks
	// readLoop.
	select {
	case <-c.done:
	case <-time.After(closeGrace):
		if err := c.cmd.Process.Kill(); err != nil {
			slog.Debug("mcp: kill after grace", "err", err)
		}
		// SIGKILL is unblockable and closes the process's stdout, so
		// readLoop's scanner hits EOF and closes c.done. Bound anyway
		// against a process stuck in uninterruptible sleep; if c.done
		// still hasn't closed, proceed to Wait as a best effort.
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			slog.Warn("mcp: readLoop did not exit after kill")
		}
	}

	return c.cmd.Wait()
}
