package contracttest

import (
	"context"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// happyProvider is a minimal in-process provider that satisfies the
// [llm.Provider] contract. Each Stream call emits two token events
// followed by a Done=true terminator. Used as the canonical conforming
// implementation to verify the fixture itself is wired correctly.
type happyProvider struct{}

// Stream returns a closed channel pre-loaded with a two-token stream
// terminated by Done=true.
func (happyProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Token: "hello "}
	ch <- llm.StreamEvent{Token: "world"}
	ch <- llm.StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

// TestProvider_HappyImplementationPasses confirms the fixture passes a
// canonical conforming provider. Negative-path verification (the
// fixture flags a violating provider with a useful message) is a manual
// development discipline: temporarily swap happyProvider for a broken
// variant and rerun this test, then revert. Automating it inside go
// test requires either mocking *testing.T (concrete type, not an
// interface) or accepting the noise of a failing subtest at every CI
// run — both worse than the manual swap.
func TestProvider_HappyImplementationPasses(t *testing.T) {
	Provider(t, func() llm.Provider { return happyProvider{} })
}
