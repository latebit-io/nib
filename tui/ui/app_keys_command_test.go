package ui

import (
	"context"
	"testing"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
	"github.com/latebit-io/nib/kit/cmdallow"
	"github.com/latebit-io/nib/tui/editor"
)

// fakeAgentPort satisfies the session's agent port so key-routing tests
// can observe approve/reject signals without a real agent (tui must not
// import coding/agent — the depguard fence).
type fakeAgentPort struct {
	approved  bool
	rejected  bool
	cancelled bool
}

func (f *fakeAgentPort) RunWithMode(context.Context, string, string, string, event.Mode) {}
func (f *fakeAgentPort) Reply(context.Context, string) bool                              { return true }
func (f *fakeAgentPort) Cancel()                                                         { f.cancelled = true }
func (f *fakeAgentPort) Approve(string)                                                  { f.approved = true }
func (f *fakeAgentPort) Reject()                                                         { f.rejected = true }

// newCommandApprovalModel builds the minimal AppModel the command
// approve/reject key paths touch: a real Session (with a fake agent
// port), an AgentPane, and an EditorModel for the reject cascade's
// completion-popup check.
func newCommandApprovalModel(t *testing.T) (*AppModel, *fakeAgentPort) {
	t.Helper()
	sess := session.New(openfile.New(buffer.New()), t.TempDir())
	fake := &fakeAgentPort{}
	sess.SetAgent(fake, make(chan event.Event, 1))
	svc := &Services{}
	return &AppModel{
		Session:   sess,
		AgentPane: NewAgentPaneModel(svc, true),
		Editor:    NewEditorModel(editor.New(buffer.New()), nil, svc),
	}, fake
}

func proposeTestCommand(m *AppModel, cmd string) {
	m.handleEngineEvent(event.AgentCommandProposed{
		Command: event.PendingCommand{ID: "c1", Command: cmd},
	})
}

func TestHandleEngineEvent_CommandProposed(t *testing.T) {
	m, _ := newCommandApprovalModel(t)
	proposeTestCommand(m, "git push")

	if m.Session.PendingCommand() == nil {
		t.Fatal("pendingCommand not staged by handleEngineEvent")
	}
	if m.AgentPane.status != event.StatusReviewing {
		t.Fatalf("status = %v, want StatusReviewing (REVIEW chip carries the approve/reject hint)", m.AgentPane.status)
	}
}

func TestApproveKeyRoutesPendingCommand(t *testing.T) {
	m, fake := newCommandApprovalModel(t)
	proposeTestCommand(m, "go test ./...")

	_, handled := m.handleGlobalAction(ActionAgentApprove)
	if !handled {
		t.Fatal("approve action not handled")
	}
	if !fake.approved {
		t.Fatal("approve key did not signal the agent")
	}
	if m.Session.PendingCommand() != nil {
		t.Fatal("approve left the command pending")
	}
}

func TestRejectKeyRoutesPendingCommand(t *testing.T) {
	m, fake := newCommandApprovalModel(t)
	proposeTestCommand(m, "git push")

	_, handled := m.handleGlobalAction(ActionAgentReject)
	if !handled {
		t.Fatal("reject action not handled")
	}
	if !fake.rejected {
		t.Fatal("reject key did not signal the agent")
	}
	if fake.cancelled {
		t.Fatal("rejecting a command must not cancel the whole run")
	}
	if m.Session.PendingCommand() != nil {
		t.Fatal("reject left the command pending")
	}
}

func TestAlwaysAllowKeyPersistsAndApproves(t *testing.T) {
	m, fake := newCommandApprovalModel(t)
	allow := cmdallow.Load(m.Session.ProjectRoot())
	m.Session.SetBashAllowlist(allow)
	proposeTestCommand(m, "go vet ./...")

	_, handled := m.handleGlobalAction(ActionAgentAlwaysAllow)
	if !handled {
		t.Fatal("always-allow action not handled with a command pending")
	}
	if !fake.approved {
		t.Fatal("always-allow did not approve the pending command")
	}
	if m.Session.PendingCommand() != nil {
		t.Fatal("always-allow left the command pending")
	}
	if !allow.Permits("go vet ./...") {
		t.Fatal("always-allow did not persist the rule")
	}
}

func TestAlwaysAllowKeyFallsThroughWithoutPending(t *testing.T) {
	m, _ := newCommandApprovalModel(t)
	if _, handled := m.handleGlobalAction(ActionAgentAlwaysAllow); handled {
		t.Fatal("always-allow with nothing pending must fall through (Option+A may be text input)")
	}
}

func TestApproveKeyNoOpWithoutPendingCommand(t *testing.T) {
	m, fake := newCommandApprovalModel(t)

	_, handled := m.handleGlobalAction(ActionAgentApprove)
	if !handled {
		t.Fatal("approve action should be handled (and swallowed) with nothing pending")
	}
	if fake.approved {
		t.Fatal("stray approve keystroke signaled the agent")
	}
}
