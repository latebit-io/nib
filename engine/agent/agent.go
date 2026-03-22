// Package agent implements the multi-turn LLM loop.
// It communicates with frontends through a typed event channel,
// making it usable from any UI framework (TUI, GUI, web, etc.).
package agent

import (
	"context"
	"encoding/json"
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

// DoneEvent signals the agent loop has finished.
// Success is true when the loop completed normally (not cancelled or errored).
type DoneEvent struct{ Success bool }

// ErrorEvent carries an error from the agent.
type ErrorEvent struct{ Err string }

// StatusEvent updates the agent status display.
type StatusEvent struct{ Status string }

func (TokenEvent) agentEvent()        {}
func (EditProposedEvent) agentEvent() {}
func (DoneEvent) agentEvent()         {}
func (ErrorEvent) agentEvent()        {}
func (StatusEvent) agentEvent()       {}

// PendingEdit is a proposed edit from the LLM, sent to the frontend for approval.
type PendingEdit struct {
	ID      string
	Search  string
	Replace string
	Reason  string
}

// Agent drives the multi-turn LLM loop.
type Agent struct {
	provider llm.Provider
	events   chan<- Event // frontend reads from this

	mu          sync.Mutex
	cancel      context.CancelFunc
	fileName    string
	fileContent string
	intent      string // current developer intent — included in every tool result

	// Approval flow: agent blocks on these channels
	approveCh  chan bool   // true = approved, false = rejected
	continueCh chan string // buffer content after user edits

	silentRetries    int
	maxSilentRetries int
}

// New creates a new agent with the given LLM provider and event channel.
// The frontend must continuously drain the events channel. Sends block
// if the channel is full, providing backpressure to the agent loop.
// Use a buffered channel (e.g. 64) to absorb bursts.
func New(provider llm.Provider, events chan<- Event) *Agent {
	return &Agent{
		provider:         provider,
		events:           events,
		approveCh:        make(chan bool, 1),
		continueCh:       make(chan string, 1),
		maxSilentRetries: 3,
	}
}

// Run starts the agent loop in a goroutine.
func (a *Agent) Run(fileName, fileContent, goal string) {
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
	a.fileName = fileName
	a.fileContent = fileContent
	a.intent = goal
	a.silentRetries = 0
	a.mu.Unlock()

	go a.run(ctx, fileName, fileContent, goal)
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

// Continue signals the user is done editing and sends the current buffer content.
func (a *Agent) Continue(bufferContent string) {
	a.mu.Lock()
	a.fileContent = bufferContent
	a.mu.Unlock()
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

func (a *Agent) run(ctx context.Context, fileName, fileContent, goal string) {
	success := false
	defer func() { a.send(DoneEvent{Success: success}) }()

	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	messages := buildMessages(fileName, fileContent, goal)
	a.send(TokenEvent{Text: "Thinking...\n\n"})
	a.send(StatusEvent{Status: "thinking"})

	thinkState := false

	for {
		ch, err := a.provider.Stream(ctx, messages)
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
			result := a.handleToolCall(ctx, tc)
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

func (a *Agent) handleToolCall(ctx context.Context, tc llm.ToolCall) string {
	name := strings.ToLower(tc.Function.Name)
	switch name {
	case "read_file":
		return a.handleReadFile()
	case "edit_file":
		return a.handleEditFile(ctx, tc)
	default:
		return fmt.Sprintf("Error: unknown tool %q", tc.Function.Name)
	}
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

func (a *Agent) handleReadFile() string {
	a.mu.Lock()
	content := a.fileContent
	a.mu.Unlock()
	return content + a.intentReminder()
}

type editArgs struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
	Reason  string `json:"reason"`
}

func (a *Agent) handleEditFile(ctx context.Context, tc llm.ToolCall) string {
	var args editArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Search == "" {
		return "Error: search field cannot be empty"
	}

	a.mu.Lock()
	content := a.fileContent
	retries := a.silentRetries
	maxRetries := a.maxSilentRetries
	a.mu.Unlock()

	// Silent retry: validate search text before presenting to user.
	// Check for exact match count — 0 means not found, >1 means ambiguous.
	matchCount := strings.Count(content, args.Search)
	if matchCount != 1 {
		a.mu.Lock()
		if retries < maxRetries {
			a.silentRetries++
			remaining := maxRetries - a.silentRetries
			a.mu.Unlock()
			var errMsg string
			if matchCount == 0 {
				errMsg = "search text not found in file"
			} else {
				errMsg = fmt.Sprintf("search text matches %d locations (expected exactly 1) — make the search text more specific", matchCount)
			}
			slog.Info("edit_file: silent retry", "reason", errMsg,
				"attempt", retries+1, "search_len", len(args.Search))
			return fmt.Sprintf("Error: %s. You have %d retries left. Read the file content carefully and copy the exact text.\n\nCurrent file:\n\n%s",
				errMsg, remaining, content) + a.intentReminder()
		}
		a.silentRetries = 0
		a.mu.Unlock()
		slog.Warn("edit_file: validation failed after max retries",
			"matches", matchCount, "search_len", len(args.Search))
		return fmt.Sprintf("Error: search text validation failed after %d retries. Use read_file to re-read the file and copy the exact text.\n\nCurrent file:\n\n%s",
			maxRetries, content) + a.intentReminder()
	}

	a.mu.Lock()
	a.silentRetries = 0
	a.mu.Unlock()

	// Send proposed edit to frontend
	a.send(StatusEvent{Status: "waiting"})
	a.send(EditProposedEvent{Edit: PendingEdit{
		ID:      tc.ID,
		Search:  args.Search,
		Replace: args.Replace,
		Reason:  args.Reason,
	}})

	// Wait for approval or rejection
	select {
	case <-ctx.Done():
		return "Error: agent canceled"
	case approved := <-a.approveCh:
		if !approved {
			a.send(StatusEvent{Status: "thinking"})
			a.send(TokenEvent{Text: "\n[Edit rejected]\n\n"})
			a.mu.Lock()
			content = a.fileContent
			a.mu.Unlock()
			return fmt.Sprintf("The developer rejected this edit. Try a different approach or move on.\n\nCurrent file:\n\n%s", content) + a.intentReminder()
		}
	}

	// Approved — wait for user to finish editing and continue
	a.send(StatusEvent{Status: "editing"})
	a.send(TokenEvent{Text: "\n[Edit approved — waiting for continue]\n"})

	select {
	case <-ctx.Done():
		return "Error: agent canceled"
	case newContent := <-a.continueCh:
		a.mu.Lock()
		a.fileContent = newContent
		a.mu.Unlock()
		a.send(StatusEvent{Status: "thinking"})
		a.send(TokenEvent{Text: "\n"})
		return fmt.Sprintf("Edit applied successfully. The developer may have made additional changes.\n\nCurrent file:\n\n%s", newContent) + a.intentReminder()
	}
}
