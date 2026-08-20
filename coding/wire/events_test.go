package wire

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
)

// stubProvider never streams; the forwarder test never runs a turn.
type stubProvider struct{}

func (stubProvider) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("stub provider does not stream")
}

type stubWorkspace struct{}

func (stubWorkspace) ReadFile(string) (string, error) { return "", nil }
func (stubWorkspace) ListFiles() ([]string, error)    { return nil, nil }
func (stubWorkspace) WriteFile(_, _ string) error     { return nil }
func (stubWorkspace) CanonPath(p string) string       { return p }
func (stubWorkspace) ProjectRoot() string             { return "" }

// TestForwardEvents_IdleCancelClosesSubscription: cancelling ctx while
// no event is in flight must still stop the forwarder and close its
// subscription; a plain range over the inbox would park forever, leaving
// the subscription registered. The agent's subscriber count is the
// observable — it drops to zero only once sub.Close() has run.
func TestForwardEvents_IdleCancelClosesSubscription(t *testing.T) {
	ag := agent.New(stubProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)

	ctx, cancel := context.WithCancel(context.Background())
	if err := ForwardEvents(ctx, ag, make(chan event.Event)); err != nil {
		t.Fatal(err)
	}
	if got := ag.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount after ForwardEvents = %d, want 1", got)
	}
	cancel()

	const timeout = 2 * time.Second
	deadline := time.Now().Add(timeout)
	for ag.SubscriberCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("subscription still registered %v after idle cancel", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestForwardEvents_ClosedAgent(t *testing.T) {
	ag := agent.New(stubProvider{}, stubWorkspace{}, nil)
	ag.Close()
	if err := ForwardEvents(context.Background(), ag, make(chan event.Event)); !errors.Is(err, agent.ErrAgentClosed) {
		t.Fatalf("ForwardEvents on closed agent = %v, want ErrAgentClosed", err)
	}
}
