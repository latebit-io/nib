// Package agent implements the multi-turn LLM loop.
// It communicates with frontends through a typed event channel,
// making it usable from any UI framework (TUI, GUI, web, etc.).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/lang"
	"github.com/latebit-io/junto/engine/llm"
)

// Agent drives the multi-turn LLM loop.
type Agent struct {
	provider llm.Provider
	events   chan<- event.Event // frontend reads from this
	tools    map[string]Tool
	toolDefs []llm.ToolDef
	cache    *FileCache
	prompts  *PromptLoader

	mu         sync.Mutex
	cancel     context.CancelFunc
	activeFile string
	intent     string // current developer intent — included in every tool result

	// Approval flow: agent blocks on these channels
	approveCh  chan bool   // true = approved, false = rejected
	continueCh chan string // buffer content after user edits
}

// NewOptions holds optional dependencies for agent construction.
type NewOptions struct {
	// DiagProvider enables diagnostics tool and auto-injection after edits.
	// Nil when no language service is available.
	DiagProvider lang.DiagnosticProvider
}

// New creates an agent with the given provider, workspace, and tools.
// The project root is derived from workspace.ProjectRoot().
// The opts parameter is optional — pass nil for defaults.
// The frontend must continuously drain the events channel. Streaming events
// (AgentToken, AgentStatus) are dropped when the channel is full; control-flow
// events (EditProposed, Done, Error) block for up to 5 seconds before being
// discarded with a log. Use a buffered channel (e.g. 64) to absorb bursts.
func New(provider llm.Provider, workspace Workspace, events chan<- event.Event, opts *NewOptions, extraTools ...Tool) *Agent {
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	cache := NewFileCache()
	projectRoot := workspace.ProjectRoot()

	var diagProvider lang.DiagnosticProvider
	if opts != nil {
		diagProvider = opts.DiagProvider
	}

	a := &Agent{
		provider:   provider,
		events:     events,
		cache:      cache,
		prompts:    NewPromptLoader(projectRoot),
		approveCh:  approveCh,
		continueCh: continueCh,
	}

	editTool := NewEditFileTool(workspace, cache, approveCh, continueCh, a.send, diagProvider)

	// Build tool registry — each tool gets exactly the dependencies it needs.
	// Built-in tools are registered first and cannot be overridden by extraTools.
	builtins := []Tool{
		NewReadFileTool(workspace, cache),
		editTool,
		NewWriteFileTool(workspace, cache, a.send),
		NewListFilesTool(workspace),
		NewBashTool(projectRoot),
	}

	// Register diagnostics tool if provider is available.
	if diagProvider != nil {
		builtins = append(builtins, NewDiagnosticsTool(diagProvider, workspace))
	}

	// Navigation tool — always available, sends events to the frontend.
	builtins = append(builtins, NewGoToLineTool(workspace, a.send))

	// Project-wide search — ripgrep with Go fallback.
	builtins = append(builtins, NewSearchProjectTool(projectRoot))

	// LSP-powered tools — conditionally registered via type assertion.
	if diagProvider != nil {
		if rp, ok := diagProvider.(lang.ReferenceProvider); ok {
			builtins = append(builtins, NewFindReferencesTool(workspace, rp))
		}
		if sp, ok := diagProvider.(lang.SymbolProvider); ok {
			builtins = append(builtins, NewWorkspaceSymbolsTool(workspace, sp))
		}
	}

	a.tools = make(map[string]Tool, len(builtins)+len(extraTools))
	a.toolDefs = make([]llm.ToolDef, 0, len(builtins)+len(extraTools))

	// Register built-ins.
	builtinNames := make(map[string]bool, len(builtins))
	for _, t := range builtins {
		def := t.Definition()
		key := strings.ToLower(def.Function.Name)
		builtinNames[key] = true
		a.tools[key] = t
		a.toolDefs = append(a.toolDefs, def)
	}

	// Register extra tools (e.g. MCP), rejecting any that shadow built-ins.
	for _, t := range extraTools {
		def := t.Definition()
		key := strings.ToLower(def.Function.Name)
		if builtinNames[key] {
			slog.Warn("extra tool rejected: shadows built-in", "name", def.Function.Name)
			continue
		}
		if _, exists := a.tools[key]; exists {
			slog.Warn("extra tool collision, skipping duplicate", "name", def.Function.Name)
			continue
		}
		a.tools[key] = t
		a.toolDefs = append(a.toolDefs, def)
	}

	return a
}

// Run starts the agent loop in a goroutine.
// contextFiles lists the files the agent is allowed to edit.
func (a *Agent) Run(fileName, fileContent, goal string, contextFiles []string) {
	a.mu.Lock()
	// Cancel any previous run
	if a.cancel != nil {
		a.cancel()
	}
	// Drain stale signals from previous run
	drain(a.approveCh)
	drain(a.continueCh)

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.activeFile = fileName
	a.cache.Reset(fileName, fileContent)
	a.intent = goal

	// Reset tools with state
	for _, t := range a.tools {
		if r, ok := t.(Resettable); ok {
			r.Reset()
		}
	}
	a.mu.Unlock()

	go a.run(ctx, fileName, fileContent, goal, contextFiles)
}

func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Approve signals that the user approved the pending edit.
func (a *Agent) Approve() {
	select {
	case a.approveCh <- true:
	default:
	}
}

// Reject signals that the user rejected the pending edit.
func (a *Agent) Reject() {
	select {
	case a.approveCh <- false:
	default:
	}
}

// Continue signals the user is done editing and sends the current buffer content
// for the file that was just edited.
func (a *Agent) Continue(path, bufferContent string) {
	slog.Debug("agent.Continue", "path", path, "content_len", len(bufferContent))
	a.cache.Set(path, bufferContent)
	select {
	case a.continueCh <- bufferContent:
	default:
	}
}

// Cancel stops the current agent run.
func (a *Agent) Cancel() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
}

// send delivers an event to the frontend. High-volume display events
// (tokens, status) are best-effort: dropped with a warning if the channel
// is full. Control-flow events (edit proposed, done, error) use a timeout
// to prevent indefinite blocking if the frontend stops draining.
func (a *Agent) send(ev event.Event) {
	switch ev.(type) {
	case event.AgentToken, event.AgentStatus:
		select {
		case a.events <- ev:
		default:
			slog.Warn("dropping agent event: channel full", "type", fmt.Sprintf("%T", ev))
		}
	default:
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case a.events <- ev:
		case <-timer.C:
			slog.Error("failed to deliver agent event: channel full", "type", fmt.Sprintf("%T", ev))
		}
	}
}

func (a *Agent) run(ctx context.Context, fileName, fileContent, goal string, contextFiles []string) {
	success := false
	defer func() { a.send(event.AgentDone{Success: success}) }()

	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	messages := a.buildMessages(fileName, fileContent, goal, contextFiles)
	a.send(event.AgentToken{Text: "Thinking...\n\n"})
	a.send(event.AgentStatus{Status: "thinking"})

	thinkState := false

	for {
		ch, err := a.provider.Stream(ctx, messages, a.toolDefs)
		if err != nil {
			slog.Error("stream failed", "err", err)
			a.send(event.AgentError{Err: fmt.Sprintf("LLM error: %v", err)})
			return
		}

		var contentBuf strings.Builder
		var toolCalls []llm.ToolCall

		for ev := range ch {
			if ev.Done {
				toolCalls = ev.ToolCalls
				break
			}

			clean := stripThinkTags(ev.Token, &thinkState)
			if clean != "" {
				contentBuf.WriteString(clean)
				a.send(event.AgentToken{Text: clean})
			}
		}

		if ctx.Err() != nil {
			return
		}

		assistantMsg := llm.Message{
			Role:    "assistant",
			Content: contentBuf.String(),
		}
		if len(toolCalls) > 0 {
			assistantMsg.ToolCalls = toolCalls
		}
		messages = append(messages, assistantMsg)

		if len(toolCalls) == 0 {
			success = true
			break
		}

		for _, tc := range toolCalls {
			if err := a.flushDirtyBuffers(ctx); err != nil {
				a.send(event.AgentError{Err: fmt.Sprintf("autosave failed: %v", err)})
				return
			}
			slog.Debug("tool call", "name", tc.Function.Name, "id", tc.ID)
			a.send(event.AgentToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})
			result := a.dispatchTool(ctx, tc)
			if ctx.Err() != nil {
				return
			}
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}
}

// flushDirtyBuffers asks the frontend to save all dirty buffers to disk,
// then invalidates the corresponding cache entries. The actual I/O runs
// on the frontend's goroutine (via FlushBuffers event) so we never touch
// TUI-owned buffer state from the agent goroutine. Called before each
// tool dispatch — not once per batch — because an earlier tool (e.g.
// edit_file) may modify buffers that a later tool needs on disk.
func (a *Agent) flushDirtyBuffers(ctx context.Context) error {
	resultCh := make(chan event.FlushResult, 1)

	// Enqueue with a bounded timeout — if the frontend isn't draining
	// events, fail visibly rather than blocking the agent indefinitely.
	enqueueTimeout := time.NewTimer(5 * time.Second)
	defer enqueueTimeout.Stop()
	select {
	case a.events <- event.FlushBuffers{Result: resultCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-enqueueTimeout.C:
		return fmt.Errorf("autosave: event queue not draining")
	}

	// Wait for the frontend to complete the save.
	responseTimeout := time.NewTimer(5 * time.Second)
	defer responseTimeout.Stop()
	select {
	case res := <-resultCh:
		for _, p := range res.Saved {
			a.cache.Invalidate(p)
		}
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	case <-responseTimeout.C:
		return fmt.Errorf("autosave: frontend response timed out")
	}
}

func (a *Agent) dispatchTool(ctx context.Context, tc llm.ToolCall) string {
	name := strings.ToLower(tc.Function.Name)
	tool, ok := a.tools[name]
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Function.Name)
	}
	result := tool.Execute(ctx, tc)
	return result + a.intentReminder()
}

// intentReminder returns a string reminding the LLM of the current intent.
// Appended to tool results so the LLM sees it every turn.
func (a *Agent) intentReminder() string {
	a.mu.Lock()
	intent := a.intent
	a.mu.Unlock()
	if intent == "" {
		return ""
	}
	return "\n\nReminder — developer's intent: " + intent
}
