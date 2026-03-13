package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/latebit/junto/protocol"
	"github.com/latebit/junto/server/internal/socket"
)

func main() {
	srv, err := socket.NewServer(handleMessage)
	if err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
	defer srv.Close()

	// Print socket path so bridge/tests can find it.
	fmt.Println(srv.SockPath())

	// Clean shutdown on signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		srv.Close()
		os.Exit(0)
	}()

	if err := srv.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// handleMessage dispatches incoming messages and manages the Phase 1 hardcoded flow.
func handleMessage(client *socket.Client, msg any) {
	// nil msg = new client connected. Send a hardcoded pending_op.
	if msg == nil {
		op := protocol.PendingOpMsg{
			Type: protocol.TypePendingOp,
			Op: protocol.EditOp{
				ID:   "phase1-test",
				Kind: "insert",
				Line: 3,
				Col:  0,
				Text: "// TODO: implement Verifier interface\n",
				Reason: "Phase 1 test — hardcoded insert to verify the full path",
			},
		}
		if err := client.Send(op); err != nil {
			log.Printf("failed to send pending_op: %v", err)
		}
		log.Printf("sent hardcoded pending_op to new client")
		return
	}

	switch m := msg.(type) {
	case *protocol.ApproveMsg:
		log.Printf("received approve for op %s", m.OpID)
		client.Send(protocol.ApprovedMsg{
			Type: protocol.TypeApproved,
			OpID: m.OpID,
		})

	case *protocol.RejectMsg:
		log.Printf("received reject for op %s", m.OpID)
		client.Send(protocol.RejectedMsg{
			Type: protocol.TypeRejected,
			OpID: m.OpID,
		})

	default:
		log.Printf("unhandled message: %T", msg)
	}
}
