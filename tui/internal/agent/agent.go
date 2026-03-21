// Package agent implements the multi-turn LLM loop for the TUI.
// Instead of socket IPC, it communicates with the Bubble Tea event loop
// through channels and tea.Cmd messages.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/latebit-io/junto/tui/internal/llm"
)

// PendingEdit is a proposed edit from the LLM, sent to the TUI for approval.
type PendingEdit struct {
	ID      string
	Search  string
	Replace string
	Reason  string
}

// --- Bubble Tea messages ---

// TokenMsg delivers streaming text to the agent pane.
type TokenMsg struct{ Text string }

// EditProposedMsg is sent when the LLM proposes an edit.
type EditProposedMsg struct{ Edit PendingEdit }

// DoneMsg signals the agent loop has finished.
type DoneMsg struct{}

// ErrorMsg carries an error from the agent.
type ErrorMsg struct{ Err string }

// StatusMsg updates the agent status display.
type StatusMsg struct{ Status string }

// Agent drives the multi-turn LLM loop.
type Agent struct {
	Provider llm.Provider
	Program  *tea.Program // for sending messages to the TUI

	mu          sync.Mutex
	cancel      context.CancelFunc
	fileName    string
	fileContent string

	// Approval flow: agent blocks on these channels
	approveCh  chan bool   // true = approved, false = rejected
	continueCh chan string // buffer content after user edits

	silentRetries    int
	maxSilentRetries int
}

// New creates a new agent with the given LLM provider.
func New(provider llm.Provider) *Agent {
	return &Agent{
		Provider:         provider,
		approveCh:        make(chan bool, 1),
		continueCh:       make(chan string, 1),
		maxSilentRetries: 3,
	}
}

// Run starts the agent loop in a goroutine. Returns a tea.Cmd that delivers
// messages to the Bubble Tea event loop.
func (a *Agent) Run(p *tea.Program, fileName, fileContent, goal string) {
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
	a.Program = p
	a.fileName = fileName
	a.fileContent = fileContent
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

func (a *Agent) send(msg tea.Msg) {
	a.mu.Lock()
	p := a.Program
	a.mu.Unlock()
	if p != nil {
		p.Send(msg)
	}
}

func (a *Agent) run(ctx context.Context, fileName, fileContent, goal string) {
	defer func() { a.send(DoneMsg{}) }()

	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	messages := llm.BuildMessages(fileName, fileContent, goal)
	a.send(TokenMsg{Text: "Thinking...\n\n"})
	a.send(StatusMsg{Status: "thinking"})

	thinkState := false

	for {
		ch, err := a.Provider.Stream(ctx, messages)
		if err != nil {
			slog.Error("stream failed", "err", err)
			a.send(ErrorMsg{Err: fmt.Sprintf("LLM error: %v", err)})
			return
		}

		var contentBuf strings.Builder
		var toolCalls []llm.ToolCall

		for ev := range ch {
			if ev.Done {
				toolCalls = ev.ToolCalls
				break
			}

			clean := llm.StripThinkTags(ev.Token, &thinkState)
			if clean != "" {
				contentBuf.WriteString(clean)
				a.send(TokenMsg{Text: clean})
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

func (a *Agent) handleReadFile() string {
	a.mu.Lock()
	content := a.fileContent
	a.mu.Unlock()
	return content
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

	// Silent retry: validate search text exists before presenting to user
	if !strings.Contains(content, args.Search) {
		a.mu.Lock()
		if retries < maxRetries {
			a.silentRetries++
			remaining := maxRetries - a.silentRetries
			a.mu.Unlock()
			slog.Info("edit_file: search text not found, silent retry",
				"attempt", retries+1, "search_len", len(args.Search))
			return fmt.Sprintf("Error: search text not found in file. You have %d retries left. Read the file content carefully and copy the exact text.\n\nCurrent file:\n\n%s",
				remaining, content)
		}
		a.silentRetries = 0
		a.mu.Unlock()
		slog.Warn("edit_file: search text not found after max retries", "search_len", len(args.Search))
		return fmt.Sprintf("Error: search text not found in file after %d retries. Use read_file to re-read the file and copy the exact text.\n\nCurrent file:\n\n%s",
			maxRetries, content)
	}

	a.mu.Lock()
	a.silentRetries = 0
	a.mu.Unlock()

	// Send proposed edit to TUI
	a.send(StatusMsg{Status: "waiting"})
	a.send(EditProposedMsg{Edit: PendingEdit{
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
			a.send(StatusMsg{Status: "thinking"})
			a.send(TokenMsg{Text: "\n[Edit rejected]\n\n"})
			a.mu.Lock()
			content = a.fileContent
			a.mu.Unlock()
			return fmt.Sprintf("The developer rejected this edit. Try a different approach or move on.\n\nCurrent file:\n\n%s", content)
		}
	}

	// Approved — wait for user to finish editing and continue
	a.send(StatusMsg{Status: "editing"})
	a.send(TokenMsg{Text: "\n[Edit approved — waiting for continue]\n"})

	select {
	case <-ctx.Done():
		return "Error: agent canceled"
	case newContent := <-a.continueCh:
		a.mu.Lock()
		a.fileContent = newContent
		a.mu.Unlock()
		a.send(StatusMsg{Status: "thinking"})
		a.send(TokenMsg{Text: "\n"})
		return fmt.Sprintf("Edit applied successfully. The developer may have made additional changes.\n\nCurrent file:\n\n%s", newContent)
	}
}
