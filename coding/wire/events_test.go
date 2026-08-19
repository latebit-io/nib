package wire

import (
	"context"
	"errors"
	"runtime"
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

// TestForwardEvents_IdleCancelReleasesGoroutine: cancelling ctx while no
// event is in flight must still stop the forwarder (and so close its
// subscription); a plain range over the inbox would park forever.
func TestForwardEvents_IdleCancelReleasesGoroutine(t *testing.T) {
	ag := agent.New(stubProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)

	ctx, cancel := context.WithCancel(context.Background())
	before := runtime.NumGoroutine()
	if err := ForwardEvents(ctx, ag, make(chan event.Event)); err != nil {
		t.Fatal(err)
	}
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("forwarder goroutine still alive %v after idle cancel", 2*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardEvents_ClosedAgent(t *testing.T) {
	ag := agent.New(stubProvider{}, stubWorkspace{}, nil)
	ag.Close()
	if err := ForwardEvents(context.Background(), ag, make(chan event.Event)); !errors.Is(err, agent.ErrAgentClosed) {
		t.Fatalf("ForwardEvents on closed agent = %v, want ErrAgentClosed", err)
	}
}
