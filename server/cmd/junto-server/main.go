package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/latebit-io/junto/protocol"
	"github.com/latebit-io/junto/server/internal/socket"
)

func main() {
	srv, err := socket.NewServer(handleMessage)
	if err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
	var closeOnce sync.Once
	closeSrv := func() { closeOnce.Do(func() { _ = srv.Close() }) }
	defer closeSrv()

	// Print socket path so bridge/tests can find it.
	fmt.Println(srv.SockPath())

	// Clean shutdown on signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		closeSrv()
	}()

	if err := srv.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// stubStep defines one step of the stub plan.
type stubStep struct {
	description string
	reasoning   []string // tokens streamed to agent pane
	op          protocol.EditOp
}

var stubPlan = []stubStep{
	{
		description: "Add Verifier interface",
		reasoning:   []string{"Looking at ", "the code...\n", "We need ", "a Verifier ", "interface.\n"},
		op: protocol.EditOp{
			ID:     "step-1",
			Kind:   "insert",
			Line:   3,
			Col:    1,
			Text:   "type Verifier interface {\n\tVerify(data []byte) error\n}\n\n",
			Reason: "Add Verifier interface",
		},
	},
	{
		description: "Add concrete implementation",
		reasoning:   []string{"Now let's ", "add a ", "concrete type ", "that implements ", "Verifier.\n"},
		op: protocol.EditOp{
			ID:     "step-2",
			Kind:   "insert",
			Line:   7,
			Col:    1,
			Text:   "type SHA256Verifier struct{}\n\nfunc (v SHA256Verifier) Verify(data []byte) error {\n\treturn nil // TODO\n}\n\n",
			Reason: "Add SHA256Verifier struct",
		},
	},
	{
		description: "Use Verifier in main",
		reasoning:   []string{"Finally, ", "let's wire ", "it into ", "main.\n"},
		op: protocol.EditOp{
			ID:     "step-3",
			Kind:   "insert",
			Line:   17,
			Col:    1,
			Text:   "\tvar v Verifier = SHA256Verifier{}\n\t_ = v\n",
			Reason: "Wire Verifier into main()",
		},
	},
}

// clientSession tracks the step-by-step progress of a single client.
type clientSession struct {
	mu          sync.Mutex
	step        int           // current step index (0-based)
	advance     chan struct{} // signaled when approve/reject received
	proceed     chan struct{} // signaled when continue received (after editing)
	done        chan struct{} // closed on disconnect to unblock waits
	rejected    bool          // last step was rejected
	currentOpID string        // op ID currently awaiting approval
}

var (
	sessionsMu sync.Mutex
	sessions   = map[*socket.Client]*clientSession{}
)

func getSession(client *socket.Client) *clientSession {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[client]
}

// stubStream runs the multi-step plan for a client, waiting for approval between steps.
func stubStream(client *socket.Client, sess *clientSession) {
	total := len(stubPlan)

	for i, step := range stubPlan {
		sess.mu.Lock()
		sess.step = i
		sess.rejected = false
		sess.mu.Unlock()

		// Announce step in agent pane
		if err := client.Send(protocol.StepMsg{
			Type:        protocol.TypeStep,
			Index:       i + 1,
			Total:       total,
			Description: step.description,
		}); err != nil {
			log.Printf("failed to send step: %v", err)
			return
		}

		// Stream reasoning tokens to agent pane
		for _, tok := range step.reasoning {
			time.Sleep(80 * time.Millisecond)
			if err := client.Send(protocol.TokenMsg{
				Type: protocol.TypeToken,
				Text: tok,
			}); err != nil {
				log.Printf("failed to send token: %v", err)
				return
			}
		}

		// Track current op and send the code edit as a pending_op
		sess.mu.Lock()
		sess.currentOpID = step.op.ID
		sess.mu.Unlock()
		if err := client.Send(protocol.PendingOpMsg{
			Type: protocol.TypePendingOp,
			Op:   step.op,
		}); err != nil {
			log.Printf("failed to send pending_op: %v", err)
			return
		}

		// Wait for approve or reject, or disconnect.
		select {
		case <-sess.advance:
		case <-sess.done:
			return
		}

		sess.mu.Lock()
		wasRejected := sess.rejected
		sess.mu.Unlock()

		if wasRejected {
			agentPaneMsg(client, "\n[Step rejected — moving on]\n")
		} else {
			agentPaneMsg(client, "\n[Step approved — edit freely, then :junto-next to continue]\n")
			// Wait for the developer to signal continue after editing.
			select {
			case <-sess.proceed:
			case <-sess.done:
				return
			}
			agentPaneMsg(client, "\n[Continuing...]\n")
		}

		time.Sleep(300 * time.Millisecond)
	}

	agentPaneMsg(client, "\n--- Plan complete ---\n")
}

// agentPaneMsg sends a token to the agent pane.
func agentPaneMsg(client *socket.Client, text string) {
	_ = client.Send(protocol.TokenMsg{
		Type: protocol.TypeToken,
		Text: text,
	})
}

// handleMessage dispatches incoming messages and manages the step-by-step flow.
func handleMessage(client *socket.Client, msg any) {
	if _, ok := msg.(socket.ConnectMsg); ok {
		sess := &clientSession{
			advance: make(chan struct{}, 1),
			proceed: make(chan struct{}, 1),
			done:    make(chan struct{}),
		}
		sessionsMu.Lock()
		sessions[client] = sess
		sessionsMu.Unlock()
		go stubStream(client, sess)
		return
	}

	if _, ok := msg.(socket.DisconnectMsg); ok {
		sessionsMu.Lock()
		sess := sessions[client]
		delete(sessions, client)
		sessionsMu.Unlock()
		if sess != nil {
			close(sess.done)
		}
		log.Printf("client session cleaned up")
		return
	}

	sess := getSession(client)
	if sess == nil {
		log.Printf("no session for client, ignoring message: %T", msg)
		return
	}

	switch m := msg.(type) {
	case *protocol.ApproveMsg:
		log.Printf("received approve for op %s", m.OpID)
		sess.mu.Lock()
		if m.OpID != sess.currentOpID {
			sess.mu.Unlock()
			log.Printf("ignoring stale approve for op %s (current: %s)", m.OpID, sess.currentOpID)
			return
		}
		sess.rejected = false
		sess.mu.Unlock()
		if err := client.Send(protocol.ApprovedMsg{
			Type: protocol.TypeApproved,
			OpID: m.OpID,
		}); err != nil {
			log.Printf("failed to send approved: %v", err)
		}
		select {
		case sess.advance <- struct{}{}:
		default:
		}

	case *protocol.RejectMsg:
		log.Printf("received reject for op %s", m.OpID)
		sess.mu.Lock()
		if m.OpID != sess.currentOpID {
			sess.mu.Unlock()
			log.Printf("ignoring stale reject for op %s (current: %s)", m.OpID, sess.currentOpID)
			return
		}
		sess.rejected = true
		sess.mu.Unlock()
		if err := client.Send(protocol.RejectedMsg{
			Type: protocol.TypeRejected,
			OpID: m.OpID,
		}); err != nil {
			log.Printf("failed to send rejected: %v", err)
		}
		select {
		case sess.advance <- struct{}{}:
		default:
		}

	case *protocol.ContinueMsg:
		log.Printf("received continue")
		select {
		case sess.proceed <- struct{}{}:
		default:
		}

	default:
		log.Printf("unhandled message: %T", msg)
	}
}
