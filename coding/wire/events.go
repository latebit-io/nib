package wire

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
)

// forwarderBufferSize is the subscription inbox capacity for the bus→chan
// forwarder. Matches the shared events channel both binaries allocate.
const forwarderBufferSize = 128

// ForwardEvents subscribes to ag's event bus and spawns a goroutine that
// copies every event onto events — the single merged stream LSP also
// writes to and the frontend (TUI or headless runner) reads. The
// goroutine exits when the subscription's inbox closes (on
// [agent.Agent.Close]) or ctx is cancelled, whichever first.
//
// Returns the Subscribe error (e.g. [agent.ErrAgentClosed]) instead of
// panicking so the caller can surface it — in nib-code the model
// switcher reports it to the user; in nib-agent it is a setup failure.
func ForwardEvents(ctx context.Context, ag *agent.Agent, events chan<- event.Event) error {
	sub, err := ag.Subscribe(agent.SubscribeOptions{BufferSize: forwarderBufferSize})
	if err != nil {
		return fmt.Errorf("agent.Subscribe: %w", err)
	}
	go func() {
		for ev := range sub.Events() {
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}
