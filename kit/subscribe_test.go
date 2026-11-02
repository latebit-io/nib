package kit

import (
	"errors"
	"testing"

	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/eventbus"
)

// TestIsStreamingEvent locks kit's layer-owned classifier.
func TestIsStreamingEvent(t *testing.T) {
	tests := []struct {
		ev   event.Event
		want bool
	}{
		{event.AgentToken{}, true},
		{event.AgentTurnUsage{}, true},
		{event.AgentInputEstimate{}, true},
		{event.AgentStatus{}, false},
		{event.AgentCompacted{}, false},
		{event.AgentDone{}, false},
	}
	for _, test := range tests {
		if got := isStreamingEvent(test.ev); got != test.want {
			t.Errorf("isStreamingEvent(%T) = %t; want %t", test.ev, got, test.want)
		}
	}
}

// TestAgentSubscribeDelegatesPolicyAndErrors verifies the kit facade.
func TestAgentSubscribeDelegatesPolicyAndErrors(t *testing.T) {
	a := &Agent{bus: eventbus.New(isStreamingEvent)}
	sub, err := a.Subscribe(SubscribeOptions{BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	a.bus.Publish(event.AgentToken{Text: "first"})
	a.bus.Publish(event.AgentToken{Text: "dropped"})
	if got := sub.Drops(); got != 1 {
		t.Errorf("Drops = %d; want 1", got)
	}

	if _, err := a.Subscribe(SubscribeOptions{OnControlFull: DropPolicy(99)}); !errors.Is(err, eventbus.ErrInvalidPolicy) {
		t.Errorf("invalid policy error = %v; want eventbus.ErrInvalidPolicy", err)
	}
	a.bus.Close()
	if _, err := a.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe after bus close error = %v; want ErrClosed", err)
	}
}
