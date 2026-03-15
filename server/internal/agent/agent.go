package agent

import (
	"context"
	"fmt"
	"log"
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

// Run starts the agent loop: sends file to LLM, streams reasoning, pauses on ops.
func (a *Agent) Run(ctx context.Context, fileName, fileContent, goal string) {
	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	messages := llm.BuildMessages(fileName, fileContent, goal)

	a.sendToken("Agent thinking...\n\n")

	ch, err := a.Provider.Stream(ctx, messages)
	if err != nil {
		log.Printf("agent: stream error: %v", err)
		a.sendError(fmt.Sprintf("LLM error: %v", err))
		return
	}

	parser := &llm.Parser{}
	stepNum := 0

	for ev := range ch {
		if ev.Done {
			break
		}

		reasoning, op := parser.Feed(ev.Token)
		if reasoning != "" {
			a.sendToken(reasoning)
		}

		if op != nil {
			stepNum++
			a.handleOp(op, stepNum)
		}
	}

	// Flush any remaining text
	if remaining := parser.Flush(); remaining != "" {
		a.sendToken(remaining)
	}

	a.sendToken("\n--- Plan complete ---\n")
}

func (a *Agent) handleOp(op *protocol.EditOp, stepNum int) {
	// Track current op
	a.Session.Mu.Lock()
	a.Session.CurrentOpID = op.ID
	a.Session.Rejected = false
	a.Session.Mu.Unlock()

	// Send pending op to plugin
	if err := a.Client.Send(protocol.PendingOpMsg{
		Type: protocol.TypePendingOp,
		Op:   *op,
	}); err != nil {
		log.Printf("agent: failed to send pending_op: %v", err)
		return
	}

	// Wait for approve/reject or disconnect
	select {
	case <-a.Session.Advance:
	case <-a.Session.Done:
		return
	}

	a.Session.Mu.Lock()
	wasRejected := a.Session.Rejected
	a.Session.Mu.Unlock()

	if wasRejected {
		a.sendToken("\n[Step rejected — moving on]\n")
	} else {
		a.sendToken("\n[Step approved — edit freely, then :junto-next to continue]\n")
		// Wait for continue signal
		select {
		case <-a.Session.Proceed:
		case <-a.Session.Done:
			return
		}
		a.sendToken("\n[Continuing...]\n")
	}
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
