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

// New creates a new agent with the given LLM provider, workspace, and event channel.
// projectRoot is the absolute path to the project directory, used to resolve
// prompt overrides from .project/prompts/ and as the working directory for bash.
// The frontend must continuously drain the events channel. Sends block
// if the channel is full, providing backpressure to the agent loop.
// Use a buffered channel (e.g. 64) to absorb bursts.
func New(provider llm.Provider, workspace Workspace, events chan<- event.Event, projectRoot string, extraTools ...Tool) *Agent {
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	cache := NewFileCache()

	a := &Agent{
		provider:   provider,
		events:     events,
		cache:      cache,
		prompts:    NewPromptLoader(projectRoot),
		approveCh:  approveCh,
		continueCh: continueCh,
	}

	// Build tool registry — each tool gets exactly the dependencies it needs.
	// Built-in tools are registered first and cannot be overridden by extraTools.
	builtins := []Tool{
		NewReadFileTool(workspace, cache),
		NewEditFileTool(workspace, cache, approveCh, continueCh, a.send),
		NewWriteFileTool(workspace, cache, a.send),
		NewListFilesTool(workspace),
		NewBashTool(projectRoot),
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

// SetDiagnosticProvider wires language diagnostics into the agent.
// Registers a diagnostics tool and enables auto-injection of diagnostics
// after approved edits. Call after New, before Run. Only call if the
// language service supports lang.DiagnosticProvider (checked in main.go).
func (a *Agent) SetDiagnosticProvider(dp lang.DiagnosticProvider, workspace Workspace) {
	// Register diagnostics tool.
	diagTool := NewDiagnosticsTool(dp, workspace)
	def := diagTool.Definition()
	key := strings.ToLower(def.Function.Name)
	a.tools[key] = diagTool
	a.toolDefs = append(a.toolDefs, def)

	// Wire auto-injection into the edit tool.
	if editTool, ok := a.tools["edit_file"].(*EditFileTool); ok {
		editTool.DiagProvider = dp
	}
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
			slog.Debug("tool call", "name", tc.Function.Name, "id", tc.ID)
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
