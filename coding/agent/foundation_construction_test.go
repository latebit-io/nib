package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// noopProvider is a minimal [llm.Provider] used by construction tests
// that never actually drive a turn. Returns an immediately-closed
// stream so a stray Stream call (e.g. from a misbehaving hook) does
// not deadlock the test.
type noopProvider struct{}

func (noopProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// TestFoundationBuilt_FoundationAndProxyInstalled verifies sub-phase
// 8c step 1 wired the foundation into [New]: the foundation pointer is
// non-nil, the [providerProxy] is non-nil and seeded with the
// constructor's provider, and a [foundationEvents] channel exists for
// the translator goroutine to drain.
//
// Pre-cutover (steps 2+) the foundation is unused machinery — but the
// plumbing has to be in place so the swap in step 2 is purely
// substitution, not new construction.
func TestFoundationBuilt_FoundationAndProxyInstalled(t *testing.T) {
	t.Parallel()

	provider := noopProvider{}
	events := make(chan event.Event, 8)

	ag := New(provider, stubWorkspace{}, events, nil)

	if ag.foundation == nil {
		t.Errorf("Agent.foundation is nil; want non-nil after New")
	}
	if ag.providerProxy == nil {
		t.Fatalf("Agent.providerProxy is nil; want non-nil after New")
	}
	if ag.foundationEvents == nil {
		t.Errorf("Agent.foundationEvents is nil; want a non-nil channel for the translator")
	}

	// The proxy must be seeded with the constructor's provider so the
	// foundation's first Stream call uses the right model.
	ag.providerProxy.mu.RLock()
	got := ag.providerProxy.p
	ag.providerProxy.mu.RUnlock()
	if got != provider {
		t.Errorf("providerProxy.p = %v; want the constructor's provider", got)
	}
}

// TestFoundationBuilt_SetProviderSwapsProxy verifies [SetProvider]
// updates both [Agent.provider] (used by the inline run loop) and the
// [providerProxy] (used by the foundation). Without the proxy update,
// SetProvider would silently no-op for the foundation post-cutover —
// the developer would toggle a model in the TUI and see nothing change.
func TestFoundationBuilt_SetProviderSwapsProxy(t *testing.T) {
	t.Parallel()

	original := noopProvider{}
	replacement := noopProvider{}
	events := make(chan event.Event, 8)

	ag := New(original, stubWorkspace{}, events, nil)

	// Sanity: original is what the proxy holds before SetProvider.
	ag.providerProxy.mu.RLock()
	before := ag.providerProxy.p
	ag.providerProxy.mu.RUnlock()
	if before != original {
		t.Fatalf("providerProxy.p before SetProvider = %v; want %v", before, original)
	}

	ag.SetProvider(replacement)

	// After SetProvider, both fields must point at the replacement.
	if ag.currentProvider() != replacement {
		t.Errorf("currentProvider() = %v; want %v after SetProvider", ag.currentProvider(), replacement)
	}
	ag.providerProxy.mu.RLock()
	after := ag.providerProxy.p
	ag.providerProxy.mu.RUnlock()
	if after != replacement {
		t.Errorf("providerProxy.p after SetProvider = %v; want %v", after, replacement)
	}
}

// TestFoundationBuilt_ToolsMirrorAdvertisedSet verifies the foundation
// receives the same tool set the inline loop advertises. Sub-phase 8c
// step 1 builds the foundation tool slice from [Agent.toolDefs] order
// (looking each tool up in [Agent.tools]) so the foundation's internal
// toolDefs match what the inline path passes to [llm.Provider.Stream].
//
// We can't reach inside [upagent.Agent] to compare the slice directly,
// so the test approximates via the [State] surface: a fresh agent has
// no run, [State.Messages] is nil — but its zero value at least proves
// foundation.State() is callable, i.e. the foundation built without
// panicking on the tool list.
func TestFoundationBuilt_ToolsMirrorAdvertisedSet(t *testing.T) {
	t.Parallel()

	events := make(chan event.Event, 8)
	ag := New(noopProvider{}, stubWorkspace{}, events, nil)

	// Sanity-check the wrapper has its own non-empty advertised set.
	if len(ag.toolDefs) == 0 {
		t.Fatal("Agent.toolDefs is empty; expected built-in tools")
	}
	for _, def := range ag.toolDefs {
		if _, ok := ag.tools[strings.ToLower(def.Function.Name)]; !ok {
			t.Errorf("toolDefs entry %q has no matching tools map entry", def.Function.Name)
		}
	}

	state := ag.foundation.State()
	if len(state.Messages) != 0 {
		t.Errorf("fresh foundation State.Messages length = %d; want 0 before any Prompt", len(state.Messages))
	}
}
