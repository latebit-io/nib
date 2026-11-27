// Package mcp connects an agent to Model Context Protocol servers. It
// wraps the official Go SDK, which speaks revision 2026-07-28 and falls
// back to 2024-11-05 through 2025-11-25, over stdio or Streamable HTTP.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os/exec"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/elicit"
)

// legacyVersion is the newest revision that starts with initialize.
const legacyVersion = "2025-11-25"

// closeGrace bounds each wait in Close: for the session to end, and for
// a stdio server to exit before it is killed. A variable for tests.
var closeGrace = 5 * time.Second

// ErrClientClosed is returned by [Client] methods once the connection
// is gone: after [Client.Close], or when a server process has exited.
var ErrClientClosed = errors.New("mcp: client closed")

// ToolInfo describes a tool exposed by an MCP server.
type ToolInfo struct {
	// Name is the tool identifier.
	Name string
	// Description explains what the tool does.
	Description string
	// InputSchema is the JSON Schema for the tool's parameters.
	InputSchema json.RawMessage
}

// ToolResult is the outcome of a tool invocation. Text is the content
// rendered as text; IsError is the server's isError flag, set when the
// tool reports a failure the transport did not have.
type ToolResult struct {
	Text    string
	IsError bool
}

// ServerInfo is what a server said about itself when the client connected.
type ServerInfo struct {
	// Protocol is the revision in use.
	Protocol string
	// Instructions describe how to use the server, for the model.
	Instructions string
	// Tools, Prompts and Resources say which features the server offers.
	Tools, Prompts, Resources bool
}

// Change is what a server says has changed.
type Change int

const (
	// ToolsChanged: the server's tool list changed.
	ToolsChanged Change = iota
	// PromptsChanged: its prompt list changed.
	PromptsChanged
)

// Client is a connection to one MCP server. Create it with
// [NewStdioClient] or [NewHTTPClient], then call [Client.Initialize].
type Client struct {
	// transport returns a new transport; a stdio server is started
	// again when it fails the probe for the newest revision.
	transport func() sdk.Transport
	stdio     bool

	mu       sync.Mutex
	session  *sdk.ClientSession
	closed   bool
	process  *exec.Cmd // the stdio server's latest process
	ask      elicit.Asker
	onChange func(Change)
	missed   []Change // changes announced before anyone watched
}

// NewStdioClient prepares a server subprocess, started by
// [Client.Initialize]. A command that cannot be found fails here.
func NewStdioClient(command string, args []string, env []string) (*Client, error) {
	if _, err := exec.LookPath(command); err != nil {
		return nil, fmt.Errorf("mcp: start %q: %w", command, err)
	}
	c := &Client{stdio: true}
	c.transport = func() sdk.Transport {
		cmd := exec.Command(command, args...)
		if len(env) > 0 {
			cmd.Env = env
		}
		c.mu.Lock()
		c.process = cmd
		c.mu.Unlock()
		return &sdk.CommandTransport{Command: cmd, TerminateDuration: closeGrace}
	}
	return c, nil
}

// Initialize connects: newest revision first, the earlier handshake when
// the answer shows an earlier server. A stdio server that exits on the
// first question, as some earlier ones do, gets a new process and the handshake.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClientClosed
	}
	client := c.sdkClient()
	session, err := client.Connect(ctx, c.transport(), nil)
	if err != nil && c.stdio && ctx.Err() == nil && errors.Is(err, sdk.ErrConnectionClosed) {
		slog.Debug("mcp: server exited; starting it again with the earlier handshake", "err", err)
		session, err = client.Connect(ctx, c.transport(), &sdk.ClientSessionOptions{ProtocolVersion: legacyVersion})
	}
	if err != nil {
		return fmt.Errorf("mcp: connect: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// Closed while connecting: nobody will close this session otherwise.
		return errors.Join(ErrClientClosed, session.Close())
	}
	c.session = session
	return nil
}

// sdkClient builds the SDK client with this Client's handlers.
func (c *Client) sdkClient() *sdk.Client {
	return sdk.NewClient(&sdk.Implementation{Name: brand.MCPServerName, Version: version()}, &sdk.ClientOptions{
		// Set, so the SDK does not offer roots; both elicitation modes are
		// offered, and with nobody to ask the answer is "cancel".
		Capabilities: &sdk.ClientCapabilities{Elicitation: &sdk.ElicitationCapabilities{
			Form: &sdk.FormElicitationCapabilities{}, URL: &sdk.URLElicitationCapabilities{},
		}},
		ElicitationHandler: c.handleElicit,
		// No resource handler: the resource tools list live, so there is nothing to follow.
		ToolListChangedHandler:   func(context.Context, *sdk.ToolListChangedRequest) { c.changed(ToolsChanged) },
		PromptListChangedHandler: func(context.Context, *sdk.PromptListChangedRequest) { c.changed(PromptsChanged) },
	})
}

// version is the kit module's version, for clientInfo.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/latebit-io/nib/kit" {
				return dep.Version
			}
		}
	}
	return "devel"
}

// active returns the connected session, or why there is none.
func (c *Client) active() (*sdk.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return nil, ErrClientClosed
	case c.session == nil:
		return nil, errors.New("mcp: not initialized")
	}
	return c.session, nil
}

// wrap maps the SDK's closed-connection error onto [ErrClientClosed].
func wrap(err error) error {
	if errors.Is(err, sdk.ErrConnectionClosed) {
		return fmt.Errorf("%w: %w", ErrClientClosed, err)
	}
	return err
}

// Server returns what the server said about itself. It is empty before
// [Client.Initialize].
func (c *Client) Server() ServerInfo {
	session, err := c.active()
	if err != nil {
		return ServerInfo{}
	}
	res := session.InitializeResult()
	if res == nil {
		return ServerInfo{}
	}
	info := ServerInfo{Protocol: res.ProtocolVersion, Instructions: res.Instructions}
	if caps := res.Capabilities; caps != nil {
		info.Tools, info.Prompts, info.Resources = caps.Tools != nil, caps.Prompts != nil, caps.Resources != nil
	}
	return info
}

// Watch sets what is called when the server says a list changed. It is
// called on the SDK's goroutine and must not block. Changes announced
// before Watch are passed to it at once.
func (c *Client) Watch(onChange func(Change)) {
	c.mu.Lock()
	c.onChange = onChange
	missed := c.missed
	c.missed = nil
	c.mu.Unlock()
	for _, change := range missed {
		onChange(change)
	}
}

// changed passes a change to the watcher, or keeps it for one.
func (c *Client) changed(change Change) {
	c.mu.Lock()
	onChange := c.onChange
	if onChange == nil && !slices.Contains(c.missed, change) {
		c.missed = append(c.missed, change)
	}
	c.mu.Unlock()
	if onChange != nil {
		onChange(change)
	}
}

// ListTools discovers the tools available on the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	return collect(ctx, c, (*sdk.ClientSession).Tools, 0, func(t *sdk.Tool) (ToolInfo, error) {
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return ToolInfo{}, fmt.Errorf("mcp: tool %s: input schema: %w", t.Name, err)
		}
		return ToolInfo{Name: t.Name, Description: t.Description, InputSchema: schema}, nil
	})
}

// collect walks one of the session's paginated listings and converts
// each item; limit, when above zero, stops it early.
func collect[P, T, U any](ctx context.Context, c *Client, list func(*sdk.ClientSession, context.Context, P) iter.Seq2[T, error], limit int, convert func(T) (U, error)) ([]U, error) {
	session, err := c.active()
	if err != nil {
		return nil, err
	}
	var zero P
	var out []U
	for item, err := range list(session, ctx, zero) {
		if err != nil {
			return nil, wrap(err)
		}
		u, err := convert(item)
		if err != nil {
			return nil, err
		}
		if out = append(out, u); limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// CallTool invokes a tool and returns its text. The isError flag is
// dropped; callers that need it use [Client.CallToolResult].
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	r, err := c.CallToolResult(ctx, name, args)
	if err != nil {
		return "", err
	}
	return r.Text, nil
}

// CallToolResult invokes a tool and returns its text and isError flag.
// isError means the tool reported a failure; err means the call failed.
// A question the server asks on the way goes to the asker ([Client.SetAsker]).
func (c *Client) CallToolResult(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	session, err := c.active()
	if err != nil {
		return ToolResult{}, err
	}
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return ToolResult{}, wrap(err)
	}
	text := renderContent(res.Content)
	if text == "" && res.StructuredContent != nil {
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return ToolResult{}, fmt.Errorf("mcp: tool %s: structured content: %w", name, err)
		}
		text = string(data)
	}
	return ToolResult{Text: text, IsError: res.IsError}, nil
}

// Close ends the connection: a stdio server is stopped (killed after a
// grace period), an HTTP session is ended. Further calls return
// [ErrClientClosed]. Calling it again does nothing.
func (c *Client) Close() error {
	c.mu.Lock()
	session, process := c.session, c.process
	already := c.closed
	c.closed, c.session = true, nil
	c.mu.Unlock()
	if already || session == nil {
		return nil
	}
	// The SDK waits for calls in flight before it closes the transport,
	// so a server that stopped answering would hold Close forever.
	done := make(chan error, 1)
	go func() { done <- session.Close() }()
	select {
	case err := <-done:
		return err
	case <-time.After(closeGrace):
	}
	if process == nil || process.Process == nil {
		return errors.New("mcp: the server did not end its session in time")
	}
	if err := process.Process.Kill(); err != nil {
		slog.Debug("mcp: kill after grace", "err", err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(closeGrace):
		return errors.New("mcp: the server did not exit after it was killed")
	}
}
