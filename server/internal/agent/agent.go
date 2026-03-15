package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/latebit-io/junto/protocol"
	"github.com/latebit-io/junto/server/internal/llm"
	"github.com/latebit-io/junto/server/internal/socket"
)

// Sender can send protocol messages to a client.
type Sender interface {
	Send(msg any) error
}

// Session provides channels for the approve/reject/continue flow.
type Session struct {
	Mu          sync.Mutex
	Advance     chan struct{} // signaled on approve/reject
	Proceed     chan struct{} // signaled on continue (junto-next)
	Done        chan struct{} // closed on disconnect
	Rejected    bool
	CurrentOpID string
	CancelRun   context.CancelFunc // cancels current agent run
}

// NewSession creates a session with initialized channels.
func NewSession() *Session {
	return &Session{
		Advance: make(chan struct{}, 1),
		Proceed: make(chan struct{}, 1),
		Done:    make(chan struct{}),
	}
}

// SetCurrentOp updates the current op ID under lock.
func (s *Session) SetCurrentOp(opID string) {
	s.Mu.Lock()
	s.CurrentOpID = opID
	s.Mu.Unlock()
}

// Agent drives the LLM loop for a single client session.
type Agent struct {
	Provider llm.Provider
	Client   Sender
	Session  *Session
}

// Run starts the multi-turn agent loop: streams reasoning, executes tool calls,
// waits for approval, feeds results back to the LLM, and repeats until done.
func (a *Agent) Run(ctx context.Context, fileName, fileContent, goal string) {
	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	messages := llm.BuildMessages(fileName, fileContent, goal)
	a.sendToken("Agent thinking...\n\n")

	stepNum := 0
	thinkState := false

	for {
		ch, err := a.Provider.Stream(ctx, messages)
		if err != nil {
			slog.Error("stream failed", "err", err)
			a.sendError(fmt.Sprintf("LLM error: %v", err))
			return
		}

		// Collect this turn's content and tool calls
		var contentBuf string
		var toolCalls []llm.ToolCall

		for ev := range ch {
			if ev.Done {
				toolCalls = ev.ToolCalls
				break
			}

			slog.Debug("token received", "token", ev.Token)
			// Strip <think> tags from reasoning
			clean := llm.StripThinkTags(ev.Token, &thinkState)
			if clean != "" {
				contentBuf += clean
				a.sendToken(clean)
			}
		}

		// Canceled?
		if ctx.Err() != nil {
			slog.Info("agent run canceled", "err", ctx.Err())
			return
		}

		// Append assistant message to conversation history
		assistantMsg := llm.Message{
			Role:    "assistant",
			Content: contentBuf,
		}
		if len(toolCalls) > 0 {
			assistantMsg.ToolCalls = toolCalls
		}
		messages = append(messages, assistantMsg)

		// No tool calls = LLM is done
		if len(toolCalls) == 0 {
			break
		}

		// Process each tool call
		for _, tc := range toolCalls {
			slog.Debug("tool call", "name", tc.Function.Name, "id", tc.ID)
			result := a.handleToolCall(ctx, tc, &stepNum)
			if ctx.Err() != nil {
				return
			}
			// Append tool result to conversation
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}

		// Loop back for next LLM turn
		a.sendToken("\n")
	}

	a.sendToken("\n--- Plan complete ---\n")
}

// editArgs is the expected JSON structure for the edit_file tool call.
type editArgs struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
	Reason  string `json:"reason"`
}

// handleToolCall processes a single tool call, sends it to the plugin for
// approval, and returns the tool result string to feed back to the LLM.
func (a *Agent) handleToolCall(ctx context.Context, tc llm.ToolCall, stepNum *int) string {
	if tc.Function.Name != "edit_file" {
		slog.Warn("unknown tool call", "name", tc.Function.Name)
		return fmt.Sprintf("Error: unknown tool %q", tc.Function.Name)
	}

	var args editArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		slog.Warn("failed to parse edit_file args", "err", err, "args", tc.Function.Arguments)
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}

	if args.Search == "" {
		slog.Warn("edit_file: empty search")
		return "Error: search field cannot be empty"
	}

	*stepNum++
	opID := fmt.Sprintf("step-%d", *stepNum)

	op := protocol.EditOp{
		ID:      opID,
		Search:  args.Search,
		Replace: args.Replace,
		Reason:  args.Reason,
	}

	// Track current op
	a.Session.Mu.Lock()
	a.Session.CurrentOpID = opID
	a.Session.Mu.Unlock()

	// Send pending op to plugin
	if err := a.Client.Send(protocol.PendingOpMsg{
		Type: protocol.TypePendingOp,
		Op:   op,
	}); err != nil {
		slog.Error("failed to send pending_op", "err", err)
		return "Error: failed to send edit to plugin"
	}

	// Wait for approve/reject or disconnect
	select {
	case <-a.Session.Advance:
	case <-a.Session.Done:
		return "Error: session disconnected"
	case <-ctx.Done():
		return "Error: canceled"
	}

	a.Session.Mu.Lock()
	wasRejected := a.Session.Rejected
	a.Session.Mu.Unlock()

	if wasRejected {
		a.sendToken("\n[Step rejected — moving on]\n")
		return "The developer rejected this edit. Try a different approach or move on."
	}

	a.sendToken("\n[Step approved — edit freely, then :junto-next to continue]\n")
	// Wait for continue signal
	select {
	case <-a.Session.Proceed:
	case <-a.Session.Done:
		return "Error: session disconnected"
	case <-ctx.Done():
		return "Error: canceled"
	}
	a.sendToken("\n[Continuing...]\n")
	return "Edit applied successfully. The developer may have made additional changes."
}

func (a *Agent) sendToken(text string) {
	_ = a.Client.Send(protocol.TokenMsg{
		Type: protocol.TypeToken,
		Text: text,
	})
}

func (a *Agent) sendError(message string) {
	_ = a.Client.Send(protocol.ErrorMsg{
		Type:    protocol.TypeError,
		Message: message,
	})
}

// compile-time check that socket.Client satisfies Sender
var _ Sender = (*socket.Client)(nil)
