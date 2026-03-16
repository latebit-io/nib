package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/latebit-io/junto/protocol"
	"github.com/latebit-io/junto/server/internal/agent"
	"github.com/latebit-io/junto/server/internal/llm"
	"github.com/latebit-io/junto/server/internal/socket"
)

var stubMode = flag.Bool("stub", false, "use hardcoded stub plan instead of LLM")
var debugMode = flag.Bool("debug", false, "enable verbose token/op logging")

func main() {
	flag.Parse()

	// Configure slog level based on --debug flag
	logLevel := slog.LevelInfo
	if *debugMode {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})))

	srv, err := socket.NewServer(handleMessage)
	if err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
	var closeOnce sync.Once
	closeSrv := func() { closeOnce.Do(func() { _ = srv.Close() }) }
	defer closeSrv()

	// Print socket path so bridge/tests can find it.
	fmt.Println(srv.SockPath())

	if !*stubMode {
		if os.Getenv("LLM_API_KEY") == "" && os.Getenv("MINIMAX_API_KEY") == "" {
			log.Println("warning: LLM_API_KEY not set, use --stub for testing without API key")
			log.Println("  Set LLM_API_KEY, LLM_BASE_URL, LLM_MODEL for any OpenAI-compatible provider")
			log.Println("  Or set MINIMAX_API_KEY for MiniMax (legacy)")
		}
	}

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

// ---------------------------------------------------------------------------
// Stub plan (used with --stub flag)
// ---------------------------------------------------------------------------

type stubStep struct {
	description string
	reasoning   []string
	op          protocol.EditOp
}

var stubPlan = []stubStep{
	{
		description: "Add Verifier interface",
		reasoning:   []string{"Looking at ", "the code...\n", "We need ", "a Verifier ", "interface.\n"},
		op: protocol.EditOp{
			ID:      "step-1",
			Search:  "package main\n",
			Replace: "package main\n\ntype Verifier interface {\n\tVerify(data []byte) error\n}\n",
			Reason:  "Add Verifier interface",
		},
	},
	{
		description: "Add concrete implementation",
		reasoning:   []string{"Now let's ", "add a ", "concrete type ", "that implements ", "Verifier.\n"},
		op: protocol.EditOp{
			ID:      "step-2",
			Search:  "type Verifier interface {\n\tVerify(data []byte) error\n}",
			Replace: "type Verifier interface {\n\tVerify(data []byte) error\n}\n\ntype SHA256Verifier struct{}\n\nfunc (v SHA256Verifier) Verify(data []byte) error {\n\treturn nil // TODO\n}",
			Reason:  "Add SHA256Verifier struct",
		},
	},
	{
		description: "Use Verifier in main",
		reasoning:   []string{"Finally, ", "let's wire ", "it into ", "main.\n"},
		op: protocol.EditOp{
			ID:      "step-3",
			Search:  "func main() {",
			Replace: "func main() {\n\tvar v Verifier = SHA256Verifier{}\n\t_ = v",
			Reason:  "Wire Verifier into main()",
		},
	},
}

// ---------------------------------------------------------------------------
// Session management
// ---------------------------------------------------------------------------

var (
	sessionsMu sync.Mutex
	sessions   = map[*socket.Client]*agent.Session{}
)

func getSession(client *socket.Client) *agent.Session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[client]
}

func agentPaneMsg(client *socket.Client, text string) {
	_ = client.Send(protocol.TokenMsg{
		Type: protocol.TypeToken,
		Text: text,
	})
}

// stubStream runs the hardcoded plan (--stub mode).
func stubStream(client *socket.Client, sess *agent.Session) {
	total := len(stubPlan)

	for i, step := range stubPlan {
		// Announce step
		if err := client.Send(protocol.StepMsg{
			Type:        protocol.TypeStep,
			Index:       i + 1,
			Total:       total,
			Description: step.description,
		}); err != nil {
			log.Printf("failed to send step: %v", err)
			return
		}

		// Stream reasoning tokens
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

		// Track current op and send
		sess.SetCurrentOp(step.op.ID)
		if err := client.Send(protocol.PendingOpMsg{
			Type: protocol.TypePendingOp,
			Op:   step.op,
		}); err != nil {
			log.Printf("failed to send pending_op: %v", err)
			return
		}

		// Wait for approve/reject or disconnect
		select {
		case <-sess.Advance:
		case <-sess.Done:
			return
		}

		sess.Mu.Lock()
		wasRejected := sess.Rejected
		sess.Mu.Unlock()

		if wasRejected {
			agentPaneMsg(client, "\n[Step rejected — moving on]\n")
		} else {
			agentPaneMsg(client, "\n[Step approved — edit freely, then :junto-next to continue]\n")
			select {
			case <-sess.Proceed:
			case <-sess.Done:
				return
			}
			agentPaneMsg(client, "\n[Continuing...]\n")
		}

		time.Sleep(300 * time.Millisecond)
	}

	agentPaneMsg(client, "\n--- Plan complete ---\n")
}

// ---------------------------------------------------------------------------
// Message handling
// ---------------------------------------------------------------------------

func handleMessage(client *socket.Client, msg any) {
	if _, ok := msg.(socket.ConnectMsg); ok {
		sess := agent.NewSession()
		sessionsMu.Lock()
		sessions[client] = sess
		sessionsMu.Unlock()

		if *stubMode {
			go stubStream(client, sess)
		} else {
			agentPaneMsg(client, "Connected. Send a task with :junto-send <goal>\n")
		}
		return
	}

	if _, ok := msg.(socket.DisconnectMsg); ok {
		sessionsMu.Lock()
		sess := sessions[client]
		delete(sessions, client)
		sessionsMu.Unlock()
		if sess != nil {
			sess.Mu.Lock()
			if sess.CancelRun != nil {
				sess.CancelRun()
				sess.CancelRun = nil
			}
			sess.Mu.Unlock()
			close(sess.Done)
		}
		slog.Info("client session cleaned up")
		return
	}

	sess := getSession(client)
	if sess == nil {
		log.Printf("no session for client, ignoring message: %T", msg)
		return
	}

	switch m := msg.(type) {
	case *protocol.StartMsg:
		log.Printf("received start: file=%s goal=%q", m.File, m.Goal)
		if *stubMode {
			log.Printf("ignoring start in stub mode")
			return
		}
		// Resolve provider config from env vars
		apiKey := os.Getenv("LLM_API_KEY")
		baseURL := os.Getenv("LLM_BASE_URL")
		model := os.Getenv("LLM_MODEL")
		// Fallback to legacy MiniMax env var
		if apiKey == "" {
			apiKey = os.Getenv("MINIMAX_API_KEY")
			if baseURL == "" {
				baseURL = "https://api.minimax.io/v1"
			}
			if model == "" {
				model = "MiniMax-M2.5"
			}
		}
		if apiKey == "" {
			agentPaneMsg(client, "[Error: LLM_API_KEY not set]\n")
			return
		}
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
		if model == "" {
			model = "anthropic/claude-sonnet-4"
		}
		// Cancel any previous run and drain channels
		sess.Mu.Lock()
		if sess.CancelRun != nil {
			sess.CancelRun()
		}
		sess.CurrentOpID = ""
		sess.Rejected = false
		sess.Mu.Unlock()
		// Drain buffered channels so the new run starts clean
		select {
		case <-sess.Advance:
		default:
		}
		select {
		case <-sess.Proceed:
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		sess.Mu.Lock()
		sess.CancelRun = cancel
		sess.Mu.Unlock()
		log.Printf("using LLM: %s @ %s", model, baseURL)
		a := &agent.Agent{
			Provider: llm.NewAgentAPI(baseURL, model, apiKey),
			Client:   client,
			Session:  sess,
		}
		go a.Run(ctx, m.File, m.Content, m.Goal)

	case *protocol.ApproveMsg:
		log.Printf("received approve for op %s", m.OpID)
		sess.Mu.Lock()
		if m.OpID != sess.CurrentOpID {
			sess.Mu.Unlock()
			log.Printf("ignoring stale approve for op %s (current: %s)", m.OpID, sess.CurrentOpID)
			return
		}
		sess.Rejected = false
		sess.Mu.Unlock()
		if err := client.Send(protocol.ApprovedMsg{
			Type: protocol.TypeApproved,
			OpID: m.OpID,
		}); err != nil {
			log.Printf("failed to send approved: %v", err)
		}
		select {
		case sess.Advance <- struct{}{}:
		default:
		}

	case *protocol.RejectMsg:
		log.Printf("received reject for op %s", m.OpID)
		sess.Mu.Lock()
		if m.OpID != sess.CurrentOpID {
			sess.Mu.Unlock()
			log.Printf("ignoring stale reject for op %s (current: %s)", m.OpID, sess.CurrentOpID)
			return
		}
		sess.Rejected = true
		sess.Mu.Unlock()
		if err := client.Send(protocol.RejectedMsg{
			Type: protocol.TypeRejected,
			OpID: m.OpID,
		}); err != nil {
			log.Printf("failed to send rejected: %v", err)
		}
		select {
		case sess.Advance <- struct{}{}:
		default:
		}

	case *protocol.ContinueMsg:
		log.Printf("received continue")
		sess.Mu.Lock()
		sess.FileContent = m.Content
		sess.Mu.Unlock()
		select {
		case sess.Proceed <- struct{}{}:
		default:
		}

	default:
		log.Printf("unhandled message: %T", msg)
	}
}
