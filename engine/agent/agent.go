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

	"github.com/latebit-io/junto/engine/llm"
)

// Event is emitted by the agent loop. Frontends receive these on the Events channel.
type Event interface {
	agentEvent() // sealed marker
}

// TokenEvent delivers streaming text from the LLM.
type TokenEvent struct{ Text string }

// EditProposedEvent is sent when the LLM proposes an edit for approval.
type EditProposedEvent struct{ Edit PendingEdit }

// FileCreatedEvent is sent when the agent creates a new file.
type FileCreatedEvent struct{ Path string }

// DoneEvent signals the agent loop has finished.
// Success is true when the loop completed normally (not cancelled or errored).
type DoneEvent struct{ Success bool }

// ErrorEvent carries an error from the agent.
type ErrorEvent struct{ Err string }

// StatusEvent updates the agent status display.
type StatusEvent struct{ Status string }

func (TokenEvent) agentEvent()        {}
func (EditProposedEvent) agentEvent() {}
func (FileCreatedEvent) agentEvent()  {}
func (DoneEvent) agentEvent()         {}
func (ErrorEvent) agentEvent()        {}
func (StatusEvent) agentEvent()       {}

// PendingEdit is a proposed edit from the LLM, sent to the frontend for approval.
type PendingEdit struct {
	ID      string
	Path    string // which file this edit targets
	Search  string
	Replace string
	Reason  string
}

// Agent drives the multi-turn LLM loop.
type Agent struct {
	provider llm.Provider
	events   chan<- Event // frontend reads from this
	tools    map[string]Tool
	toolDefs []llm.ToolDef
	cache    *FileCache

	mu         sync.Mutex
	cancel     context.CancelFunc
	activeFile string
	intent     string // current developer intent — included in every tool result

	// Approval flow: agent blocks on these channels
	approveCh  chan bool   // true = approved, false = rejected
	continueCh chan string // buffer content after user edits
}

// New creates a new agent with the given LLM provider, workspace, and event channel.
// The frontend must continuously drain the events channel. Sends block
// if the channel is full, providing backpressure to the agent loop.
// Use a buffered channel (e.g. 64) to absorb bursts.
func New(provider llm.Provider, workspace Workspace, events chan<- Event) *Agent {
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	cache := NewFileCache()

	a := &Agent{
		provider:   provider,
		events:     events,
		cache:      cache,
		approveCh:  approveCh,
		continueCh: continueCh,
	}

	// Build tool registry — each tool gets exactly the dependencies it needs.
	toolList := []Tool{
		NewReadFileTool(workspace, cache),
		NewEditFileTool(workspace, cache, approveCh, continueCh, a.send),
		NewWriteFileTool(workspace, cache, a.send),
		NewListFilesTool(workspace),
	}

	a.tools = make(map[string]Tool, len(toolList))
	a.toolDefs = make([]llm.ToolDef, 0, len(toolList))
	for _, t := range toolList {
		def := t.Definition()
		a.tools[def.Function.Name] = t
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

func (a *Agent) send(ev Event) {
	a.events <- ev
}

func (a *Agent) run(ctx context.Context, fileName, fileContent, goal string, contextFiles []string) {
	success := false
	defer func() { a.send(DoneEvent{Success: success}) }()

	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	messages := buildMessages(fileName, fileContent, goal, contextFiles)
	a.send(TokenEvent{Text: "Thinking...\n\n"})
	a.send(StatusEvent{Status: "thinking"})

	thinkState := false

	for {
		ch, err := a.provider.Stream(ctx, messages, a.toolDefs)
		if err != nil {
			slog.Error("stream failed", "err", err)
			a.send(ErrorEvent{Err: fmt.Sprintf("LLM error: %v", err)})
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
				a.send(TokenEvent{Text: clean})
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
