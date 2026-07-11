package session

import (
	"testing"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/kit/cmdallow"
)

// pendCommand drives a command proposal into the session the same way
// the event loop does.
func pendCommand(s *Session, id, cmd string) {
	s.HandleEvent(event.AgentCommandProposed{
		Command: event.PendingCommand{ID: id, Command: cmd},
	})
}

func TestHandleEventCommandProposed(t *testing.T) {
	s := newTestSession("")
	if s.PendingCommand() != nil {
		t.Fatal("fresh session has a pending command")
	}
	pendCommand(s, "c1", "git status")
	pc := s.PendingCommand()
	if pc == nil || pc.ID != "c1" || pc.Command != "git status" {
		t.Fatalf("PendingCommand = %+v, want {c1 git status}", pc)
	}
}

func TestApproveCommandClearsPending(t *testing.T) {
	s := newTestSession("")
	pendCommand(s, "c1", "go test ./...")
	s.ApproveCommand()
	if s.PendingCommand() != nil {
		t.Fatal("ApproveCommand left the command pending")
	}
}

func TestRejectCommandClearsPending(t *testing.T) {
	s := newTestSession("")
	pendCommand(s, "c1", "git push")
	s.RejectCommand("user")
	if s.PendingCommand() != nil {
		t.Fatal("RejectCommand left the command pending")
	}
}

func TestApproveCommandNoOpWithoutPending(t *testing.T) {
	// A stray approve keystroke with nothing pending must not signal
	// the agent (that would queue a stale approval on the coordinator).
	s := newTestSession("")
	s.ApproveCommand()
	s.RejectCommand("user")
	if s.PendingCommand() != nil {
		t.Fatal("no-op approve/reject materialized a pending command")
	}
}

func TestAlwaysAllowCommandPersistsRule(t *testing.T) {
	root := t.TempDir()
	s := newTestSessionWithRoot("", root)
	allow := cmdallow.Load(root)
	s.SetBashAllowlist(allow)
	pendCommand(s, "c1", "go test ./...")

	if err := s.AlwaysAllowCommand(); err != nil {
		t.Fatalf("AlwaysAllowCommand: %v", err)
	}
	if s.PendingCommand() != nil {
		t.Fatal("always-allow left the command pending")
	}
	if !allow.Permits("go test ./...") {
		t.Fatal("always-allow did not persist the rule")
	}
	if !cmdallow.Load(root).Permits("go test ./...") {
		t.Fatal("rule did not reach disk")
	}
}

func TestAlwaysAllowCommandWithoutAllowlistApprovesOnce(t *testing.T) {
	s := newTestSession("")
	pendCommand(s, "c1", "ls")

	err := s.AlwaysAllowCommand()
	if err == nil {
		t.Fatal("nil allowlist must surface an error")
	}
	if s.PendingCommand() != nil {
		t.Fatal("command must still be approved once despite the allowlist error")
	}
}

func TestAlwaysAllowCommandNoOpWithoutPending(t *testing.T) {
	s := newTestSession("")
	if err := s.AlwaysAllowCommand(); err != nil {
		t.Fatalf("no-op AlwaysAllowCommand returned %v", err)
	}
}

func TestRunBoundaryEventsClearPendingCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   event.Event
	}{
		{"AgentDone", event.AgentDone{}},
		{"AgentError", event.AgentError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSession("")
			pendCommand(s, "c1", "ls")
			s.HandleEvent(tc.ev)
			if s.PendingCommand() != nil {
				t.Fatalf("%s did not clear the pending command", tc.name)
			}
		})
	}
}
