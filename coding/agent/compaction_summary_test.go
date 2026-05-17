package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// scriptedProvider is a [llm.Provider] that returns the configured
// chunks on every Stream call and tracks how many calls it has seen.
// Tests use it to drive Tier 2 deterministically without spinning up
// a real LLM backend.
type scriptedProvider struct {
	mu        sync.Mutex
	chunks    []string
	err       error
	truncated bool
	calls     int
	lastMsgs  []llm.Message
}

func (p *scriptedProvider) Stream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	p.lastMsgs = msgs
	chunks := p.chunks
	scripted := p.err
	truncated := p.truncated
	p.mu.Unlock()

	if scripted != nil {
		return nil, scripted
	}
	ch := make(chan llm.StreamEvent, len(chunks)+1)
	go func() {
		defer close(ch)
		for _, c := range chunks {
			select {
			case ch <- llm.StreamEvent{Token: c}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- llm.StreamEvent{Done: true, Truncated: truncated}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func TestSplitForSummarization_TooShort(t *testing.T) {
	t.Parallel()
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}
	_, err := splitForSummarization(msgs)
	if !errors.Is(err, errSummarizeNothing) {
		t.Fatalf("err = %v, want errSummarizeNothing", err)
	}
}

func TestSplitForSummarization_LandsOnUserBoundary(t *testing.T) {
	t.Parallel()
	// Build a transcript with multiple user turns. The recent tail must
	// cover at least keepRecentTokens worth of content; the split index
	// must point at a user message.
	heavy := strings.Repeat("x", 50_000) // ~12.5k tokens of recent text
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ack one"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "ack two"},
		{Role: "user", Content: "recent"},
		{Role: "assistant", Content: heavy},
	}
	splitIdx, err := splitForSummarization(msgs)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if msgs[splitIdx].Role != "user" {
		t.Fatalf("split %d -> %q role, want user", splitIdx, msgs[splitIdx].Role)
	}
	// Old range must contain at least the first user message.
	if splitIdx < 2 {
		t.Fatalf("split %d too small — no old range", splitIdx)
	}
}

func TestSplitForSummarization_AutonomousModeFallsBackToAssistant(t *testing.T) {
	t.Parallel()
	// Autonomous-mode shape: one user goal at index 1, then a long
	// sequence of assistant + tool messages. Without an assistant
	// fallback, this case would never summarize.
	//
	// Each assistant carries ~2k tokens of content; tool results are
	// modest. After ~15 messages of accumulated tail, the walk has
	// covered keepRecentTokens (10k) and should pick the latest
	// assistant boundary above that cutoff.
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "build me a thing"},
	}
	heavy := strings.Repeat("body ", 500) // ~2.5k chars per assistant
	for i := 0; i < 20; i++ {
		msgs = append(msgs, llm.Message{Role: "assistant", Content: heavy})
		msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: "tc", Content: "result"})
	}
	splitIdx, err := splitForSummarization(msgs)
	if err != nil {
		t.Fatalf("autonomous-mode split should succeed; got err=%v", err)
	}
	if splitIdx <= 1 {
		t.Fatalf("split %d should be > 1", splitIdx)
	}
	if msgs[splitIdx].Role != "user" && msgs[splitIdx].Role != "assistant" {
		t.Fatalf("split lands on role %q; want user or assistant", msgs[splitIdx].Role)
	}
	if msgs[splitIdx].Role == "tool" {
		t.Fatal("split must NEVER land on a tool message — would orphan ToolCallID")
	}
}

func TestSplitForSummarization_NothingOldEnough(t *testing.T) {
	t.Parallel()
	// One user message + a recent tail. Nothing older than the most
	// recent user message exists; split must report nothing-to-summarize.
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("a", 100_000)},
		{Role: "assistant", Content: "ack"},
	}
	_, err := splitForSummarization(msgs)
	if !errors.Is(err, errSummarizeNothing) {
		t.Fatalf("err = %v, want errSummarizeNothing", err)
	}
}

func TestRenderTranscript_Shape(t *testing.T) {
	t.Parallel()
	msgs := []llm.Message{
		{Role: "user", Content: "ping"},
		{
			Role:    "assistant",
			Content: "running it",
			ToolCalls: []llm.ToolCall{
				{ID: "tc1", Function: llm.FunctionCall{Name: "bash", Arguments: `{"command":"echo hi"}`}},
			},
		},
		{Role: "tool", ToolCallID: "tc1", Content: "hi"},
	}
	got := renderTranscript(msgs)
	for _, want := range []string{
		"### USER",
		"### ASSISTANT",
		"### TOOL (tool_call_id=tc1)",
		"[tool_call id=tc1 name=bash args=",
		"\"command\":\"echo hi\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q\n---\n%s", want, got)
		}
	}
}

func TestRunSummarization_NilProvider(t *testing.T) {
	t.Parallel()
	_, err := runSummarization(context.Background(), nil, "anything")
	if err == nil {
		t.Fatal("expected error from nil provider")
	}
}

func TestRunSummarization_Success(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{chunks: []string{"developer ", "asked for X; ", "agent built Y."}}
	got, err := runSummarization(context.Background(), p, "transcript")
	if err != nil {
		t.Fatalf("runSummarization: %v", err)
	}
	want := "developer asked for X; agent built Y."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1", p.calls)
	}
	// The system + user prompt frame should be present in what
	// we sent to the provider.
	if len(p.lastMsgs) != 2 {
		t.Fatalf("provider saw %d msgs, want 2 (system + user)", len(p.lastMsgs))
	}
	if p.lastMsgs[0].Role != "system" {
		t.Errorf("msg[0].Role = %q, want system", p.lastMsgs[0].Role)
	}
	if !strings.Contains(p.lastMsgs[1].Content, "transcript") {
		t.Errorf("user msg missing transcript: %q", p.lastMsgs[1].Content)
	}
}

func TestRunSummarization_EmptyResponseIsError(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{chunks: []string{"   "}}
	_, err := runSummarization(context.Background(), p, "transcript")
	if err == nil {
		t.Fatal("expected error on empty summary")
	}
}

func TestRunSummarization_ProviderError(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{err: errors.New("upstream blew up")}
	_, err := runSummarization(context.Background(), p, "transcript")
	if err == nil {
		t.Fatal("expected error propagation")
	}
}

// buildTier2Fixture constructs a message slice that will cross the
// summarization threshold and own a valid user-boundary split. The
// recent verbatim tail is sized to satisfy [keepRecentTokens] so the
// split lands cleanly; the old range carries enough text to push
// estimated history past [summarizationThreshold].
func buildTier2Fixture() []llm.Message {
	old := strings.Repeat("old content ", 8000) // ~24k tokens of old
	recent := strings.Repeat("recent ", 6000)   // ~10k tokens recent
	return []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first goal"},
		{Role: "assistant", Content: old},
		{Role: "user", Content: "second goal"},
		{Role: "assistant", Content: old},
		{Role: "user", Content: "third — recent boundary"},
		{Role: "assistant", Content: recent},
	}
}

func TestMaybeCompact_Tier2FiresAndEmitsSummary(t *testing.T) {
	t.Parallel()
	msgs := buildTier2Fixture()
	before := llm.EstimateMessageTokens(msgs, nil)
	if before.History < summarizationThreshold {
		t.Skipf("fixture below tier-2 threshold: history=%d threshold=%d",
			before.History, summarizationThreshold)
	}

	p := &scriptedProvider{chunks: []string{"high-level summary of earlier turns"}}
	a := &Agent{bus: newBus(), provider: p}
	var out []llm.Message
	sent := drainBus(t, a, func() {
		out = a.maybeCompact(context.Background(), msgs, nil)
	})

	// Find the summarization event.
	var summaryEv *event.AgentCompactionSummary
	for i := range sent {
		if s, ok := sent[i].(event.AgentCompactionSummary); ok {
			summaryEv = &s
		}
	}
	if summaryEv == nil {
		t.Fatalf("no AgentCompactionSummary event among %d events", len(sent))
	}
	if !strings.Contains(summaryEv.Summary, "high-level summary") {
		t.Errorf("event.Summary missing scripted text: %q", summaryEv.Summary)
	}
	if summaryEv.SummarizedMessages < 1 {
		t.Errorf("event.SummarizedMessages = %d, want >= 1", summaryEv.SummarizedMessages)
	}

	// Output shape: msgs[0] preserved, exactly one assistant summary,
	// rest is the recent tail.
	if len(out) < 3 {
		t.Fatalf("len(out) = %d, want at least 3 (system + summary + recent)", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "sys" {
		t.Errorf("out[0] = %+v, want system unchanged", out[0])
	}
	if out[1].Role != "assistant" {
		t.Errorf("out[1].Role = %q, want assistant (summary)", out[1].Role)
	}
	if !strings.Contains(out[1].Content, summaryPrefix) {
		t.Errorf("out[1].Content missing summary prefix: %q", out[1].Content[:min(80, len(out[1].Content))])
	}
	// Recent verbatim: the last assistant message's content must match
	// the input's final assistant content byte-for-byte.
	if out[len(out)-1].Content != msgs[len(msgs)-1].Content {
		t.Error("final message body changed — recent tail not verbatim")
	}

	after := llm.EstimateMessageTokens(out, nil)
	if after.History >= before.History {
		t.Errorf("after.History (%d) >= before.History (%d) — Tier 2 did not shrink", after.History, before.History)
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1", p.calls)
	}
}

func TestMaybeCompact_Tier2SoftDegradesOnProviderError(t *testing.T) {
	t.Parallel()
	msgs := buildTier2Fixture()
	before := llm.EstimateMessageTokens(msgs, nil)
	if before.History < summarizationThreshold {
		t.Skipf("fixture below tier-2 threshold")
	}

	p := &scriptedProvider{err: errors.New("upstream gone")}
	a := &Agent{bus: newBus(), provider: p}
	var out []llm.Message
	sent := drainBus(t, a, func() {
		out = a.maybeCompact(context.Background(), msgs, nil)
	})

	// On Tier 2 failure, output is the post-Tier-1 slice. Since the
	// fixture has no compactable tool results, Tier 1 also won't shrink
	// anything and out should equal msgs.
	if len(out) != len(msgs) {
		t.Errorf("len(out) = %d, want %d (soft degrade should preserve slice)", len(out), len(msgs))
	}
	// No AgentCompactionSummary event must fire on failure.
	for _, ev := range sent {
		if _, ok := ev.(event.AgentCompactionSummary); ok {
			t.Fatalf("AgentCompactionSummary emitted despite provider error")
		}
	}
	if p.calls != 1 {
		t.Errorf("provider calls = %d, want 1", p.calls)
	}
}

func TestMaybeCompact_Tier2SkipsWhenNoProvider(t *testing.T) {
	t.Parallel()
	msgs := buildTier2Fixture()
	a := &Agent{bus: newBus()} // no provider
	var out []llm.Message
	sent := drainBus(t, a, func() {
		out = a.maybeCompact(context.Background(), msgs, nil)
	})
	// Output must come back unchanged; no panic; no summarization event.
	if len(out) != len(msgs) {
		t.Errorf("len(out) = %d, want %d", len(out), len(msgs))
	}
	for _, ev := range sent {
		if _, ok := ev.(event.AgentCompactionSummary); ok {
			t.Fatalf("unexpected AgentCompactionSummary when provider is nil")
		}
	}
}
