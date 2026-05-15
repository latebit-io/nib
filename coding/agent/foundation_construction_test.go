package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
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

// TestKitBuilt_KitAndProxyInstalled verifies [New] wires the kit
// agent: the kit pointer is non-nil, the [providerProxy] is non-nil
// and seeded with the constructor's provider, and a [kitEvents]
// channel exists for the forwarder goroutine to drain.
func TestKitBuilt_KitAndProxyInstalled(t *testing.T) {
	t.Parallel()

	// Pointer instance so the equality assertion below has identity
	// semantics — comparing two value-type noopProvider{} structs
	// always returns true and would mask a wiring bug.
	provider := &noopProvider{}
	ag := New(provider, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)

	if ag.kit == nil {
		t.Errorf("Agent.kit is nil; want non-nil after New")
	}
	if ag.providerProxy == nil {
		t.Fatalf("Agent.providerProxy is nil; want non-nil after New")
	}
	if ag.kitSub == nil {
		t.Errorf("Agent.kitSub is nil; want a non-nil subscription for the forwarder")
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

// TestKitBuilt_SetProviderSwapsProxy verifies [SetProvider] updates
// both [Agent.provider] and the [providerProxy] (used by the kit/
// foundation). Without the proxy update, SetProvider would silently
// no-op for the kit — the developer would toggle a model in the TUI
// and see nothing change.
func TestKitBuilt_SetProviderSwapsProxy(t *testing.T) {
	t.Parallel()

	// Pointer instances so original and replacement are distinct under
	// `==`. Value-type noopProvider{} structs all compare equal, which
	// would mask a SetProvider that silently no-ops.
	original := &noopProvider{}
	replacement := &noopProvider{}
	ag := New(original, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)

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

// TestKitBuilt_NilProviderPanicsAtConstruction locks the fast-fail
// guard on nil providers. kit.New only sees the providerProxy
// (always non-nil), so a nil [Agent.provider] would otherwise slip
// past kit's own validation and panic at first Stream — far from the
// bad call site. Failing in [Agent.buildKitAgent] surfaces the bug
// immediately.
func TestKitBuilt_NilProviderPanicsAtConstruction(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New(nil provider) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "provider is required") {
			t.Errorf("panic message = %q, want substring 'provider is required'", msg)
		}
	}()

	_ = New(nil, stubWorkspace{}, nil)
}

// TestKitBuilt_ToolsMirrorAdvertisedSet verifies the kit/foundation
// receives the same tool set the wrapper advertises. The kit tool
// slice is built from [Agent.toolDefs] order (looking each tool up in
// [Agent.tools]) so the kit's internal toolDefs match what the
// wrapper passes to [llm.Provider.Stream].
//
// We can't reach inside [kit.Agent] to compare the slice directly, so
// the test approximates via the [State] surface: a fresh agent has no
// run, [State.Messages] is nil — but its zero value at least proves
// kit.State() is callable, i.e. the kit built without panicking on
// the tool list.
func TestKitBuilt_ToolsMirrorAdvertisedSet(t *testing.T) {
	t.Parallel()

	ag := New(noopProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)

	// Sanity-check the wrapper has its own non-empty advertised set.
	if len(ag.toolDefs) == 0 {
		t.Fatal("Agent.toolDefs is empty; expected built-in tools")
	}
	for _, def := range ag.toolDefs {
		if _, ok := ag.tools[strings.ToLower(def.Function.Name)]; !ok {
			t.Errorf("toolDefs entry %q has no matching tools map entry", def.Function.Name)
		}
	}

	state := ag.kit.State()
	if len(state.Messages) != 0 {
		t.Errorf("fresh kit State.Messages length = %d; want 0 before any Prompt", len(state.Messages))
	}
}
