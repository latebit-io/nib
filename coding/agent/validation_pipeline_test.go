package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/validate"
)

// fakePipeline feeds a fixed sequence of results back to the agent so
// the retry-budget behaviour can be exercised deterministically.
type fakePipeline struct {
	queue []validate.Result
	calls int
}

func (p *fakePipeline) Run(context.Context, validate.Candidate) []validate.Result {
	p.calls++
	if len(p.queue) == 0 {
		return nil
	}
	r := p.queue[0]
	p.queue = p.queue[1:]
	return []validate.Result{r}
}

type stubProv struct{}

func (stubProv) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// newPipelineTestAgent builds a minimal agent plumbed with the given
// pipeline. Events go into a large-enough buffered channel that tests can
// drain and assert on them after the call returns.
func newPipelineTestAgent(t *testing.T, pipe validate.Pipeline) (*Agent, <-chan event.Event) {
	t.Helper()
	ws := &testWorkspace{
		root:      "",
		files:     map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		inContext: map[string]bool{},
	}
	events := make(chan event.Event, 16)
	ag := New(stubProv{}, ws, events, &NewOptions{ValidationPipeline: pipe})
	return ag, events
}

// sampleProposal returns a canonical EditProposal the retry tests can reuse.
func sampleProposal() EditProposal {
	return EditProposal{
		Edit: event.PendingEdit{
			ID: "e1", Path: "main.go", Search: "hello", Replace: "goodbye",
		},
		Path:            "main.go",
		CanonPath:       "main.go",
		ExpectedContent: "package main\n\nfunc main() {}\n",
	}
}

// TestRunValidationPipelinePassProceeds verifies that a Pass verdict
// does not produce retry feedback and returns no summaries to elide
// from the approval flow.
func TestRunValidationPipelinePassProceeds(t *testing.T) {
	t.Parallel()

	pipe := &fakePipeline{queue: []validate.Result{{Verdict: validate.Pass, Stage: "fake"}}}
	ag, _ := newPipelineTestAgent(t, pipe)

	summaries, feedback := ag.runValidationPipeline(context.Background(), sampleProposal())
	if feedback != "" {
		t.Errorf("Pass verdict returned feedback: %q", feedback)
	}
	if len(summaries) != 1 || summaries[0].Verdict != "pass" {
		t.Errorf("summaries = %+v, want one pass summary", summaries)
	}
}

// TestRunValidationPipelineRetryConsumesBudget verifies Retry verdicts
// return feedback (shortcutting the approval flow) up to the retry cap,
// then surface the proposal with summaries attached when exhausted.
func TestRunValidationPipelineRetryConsumesBudget(t *testing.T) {
	t.Parallel()

	// Queue Retry verdicts for every call the test will make.
	queue := make([]validate.Result, maxValidatorRetries+1)
	for i := range queue {
		queue[i] = validate.Result{
			Verdict:  validate.Retry,
			Stage:    "go-parse",
			Feedback: "bad parse",
		}
	}
	pipe := &fakePipeline{queue: queue}
	ag, _ := newPipelineTestAgent(t, pipe)

	// First maxValidatorRetries calls must return feedback (silent retry).
	for i := 0; i < maxValidatorRetries; i++ {
		_, feedback := ag.runValidationPipeline(context.Background(), sampleProposal())
		if feedback == "" {
			t.Errorf("attempt %d: feedback empty; expected retry prompt", i+1)
		}
	}

	// After the budget is spent the pipeline still returns Retry but the
	// agent surfaces summaries instead of retrying silently.
	summaries, feedback := ag.runValidationPipeline(context.Background(), sampleProposal())
	if feedback != "" {
		t.Errorf("exhausted budget produced feedback: %q; expected surface", feedback)
	}
	if len(summaries) != 1 || summaries[0].Verdict != "retry" {
		t.Errorf("surfaced summaries = %+v, want one retry summary", summaries)
	}
}

// TestRunValidationPipelineBlockConsumesBudget verifies Block verdicts
// flow through the same retry-budget mechanism as Retry. Without this
// behaviour a Block verdict would be theatre under autonomous mode:
// the validator reports a critical issue (architecture cap exceeded,
// must split file) but the LLM never sees the feedback and the TUI's
// LevelTrusted gate auto-approves the proposal anyway. This regression
// test was added after a Pac-Man rerun corrupted main.lua because the
// architecture validator's Block was ignored end-to-end.
func TestRunValidationPipelineBlockConsumesBudget(t *testing.T) {
	t.Parallel()

	queue := make([]validate.Result, maxValidatorRetries+1)
	for i := range queue {
		queue[i] = validate.Result{
			Verdict:  validate.Block,
			Stage:    "architecture",
			Feedback: "file too big — split it",
		}
	}
	pipe := &fakePipeline{queue: queue}
	ag, _ := newPipelineTestAgent(t, pipe)

	// First maxValidatorRetries calls must return feedback so the LLM
	// gets a chance to fix the architectural issue before the proposal
	// surfaces to the developer.
	for i := 0; i < maxValidatorRetries; i++ {
		_, feedback := ag.runValidationPipeline(context.Background(), sampleProposal())
		if feedback == "" {
			t.Errorf("attempt %d: Block returned no feedback; expected retry prompt", i+1)
		}
	}

	// Budget exhausted — proposal must surface with summaries so the
	// TUI's auto-approve gate can refuse approval.
	summaries, feedback := ag.runValidationPipeline(context.Background(), sampleProposal())
	if feedback != "" {
		t.Errorf("exhausted budget produced feedback: %q; expected surface", feedback)
	}
	if len(summaries) != 1 || summaries[0].Verdict != "block" {
		t.Errorf("surfaced summaries = %+v, want one block summary", summaries)
	}
}

// TestRunValidationPipelineNonPassWithoutFeedbackSurfaces verifies that
// a non-Pass verdict with empty Feedback short-circuits to surfacing
// the proposal rather than sending an empty retry message to the LLM
// (which would be a useless cycle of "fix this: "). It also asserts
// the retry budget is NOT consumed: empty-feedback rounds are
// developer-visible surfaces, not silent retries, and burning a slot
// would exhaust the budget on attempts that never actually retry.
func TestRunValidationPipelineNonPassWithoutFeedbackSurfaces(t *testing.T) {
	t.Parallel()

	pipe := &fakePipeline{queue: []validate.Result{
		{Verdict: validate.Block, Stage: "x", Feedback: ""},
	}}
	ag, _ := newPipelineTestAgent(t, pipe)

	proposal := sampleProposal()
	summaries, feedback := ag.runValidationPipeline(context.Background(), proposal)
	if feedback != "" {
		t.Errorf("empty-feedback non-Pass returned %q; want surface (empty)", feedback)
	}
	if len(summaries) != 1 {
		t.Errorf("summaries = %+v, want one summary surfacing the verdict", summaries)
	}
	if got, ok := ag.validatorRetries[proposal.CanonPath]; ok {
		t.Errorf("empty-feedback round consumed budget: retries[%q] = %d; want untouched",
			proposal.CanonPath, got)
	}
}

// TestRunValidationPipelineResetsOnApproval verifies recordEdit wipes the
// per-path retry counter so subsequent edits to the same file start with
// a fresh budget.
func TestRunValidationPipelineResetsOnApproval(t *testing.T) {
	t.Parallel()

	pipe := &fakePipeline{queue: []validate.Result{
		{Verdict: validate.Retry, Stage: "go-parse", Feedback: "bad"},
	}}
	ag, _ := newPipelineTestAgent(t, pipe)

	proposal := sampleProposal()
	_, feedback := ag.runValidationPipeline(context.Background(), proposal)
	if feedback == "" {
		t.Fatalf("first retry: feedback empty")
	}
	if got := ag.validatorRetries[proposal.CanonPath]; got != 1 {
		t.Errorf("retries[%q] = %d, want 1", proposal.CanonPath, got)
	}

	ag.recordEdit(proposal)
	if got, ok := ag.validatorRetries[proposal.CanonPath]; ok {
		t.Errorf("recordEdit did not delete counter: retries[%q] = %d", proposal.CanonPath, got)
	}
}

// TestRunValidationPipelineNoOpReturnsNilFast verifies the null-object
// path has zero observable effect — no summaries, no feedback, no state
// mutation — so wiring the pipeline cannot regress existing callers.
func TestRunValidationPipelineNoOpReturnsNilFast(t *testing.T) {
	t.Parallel()

	ag, _ := newPipelineTestAgent(t, validate.NoopPipeline{})
	summaries, feedback := ag.runValidationPipeline(context.Background(), sampleProposal())
	if len(summaries) != 0 || feedback != "" {
		t.Errorf("NoopPipeline path: summaries=%+v feedback=%q; want both empty", summaries, feedback)
	}
}

// alwaysRetryPipeline returns a Retry verdict on every Run call so the
// race-detector test deterministically exercises the map mutation path
// in runValidationPipeline. Separate from fakePipeline (which consumes
// a finite queue) so this test can run for an arbitrary duration.
type alwaysRetryPipeline struct{ calls atomic.Int64 }

func (p *alwaysRetryPipeline) Run(context.Context, validate.Candidate) []validate.Result {
	p.calls.Add(1)
	return []validate.Result{{
		Verdict: validate.Retry, Stage: "fake", Feedback: "retry",
	}}
}

// TestValidatorRetriesNoRace exercises the validatorRetries map
// concurrently from two mutation paths that lock on a.mu:
//
//   - runValidationPipeline (increments the per-path counter on every
//     Retry verdict)
//   - recordEdit (deletes the per-path counter on every successful
//     approval)
//
// If either path drops its a.mu guard, `go test -race` fails
// immediately with a concurrent-map-access fatal. The test is cheap
// (100 ms of hammering) but the guarantee it provides is exactly the
// regression CodeRabbit flagged — a run-time fatal if the locking
// discipline regresses.
func TestValidatorRetriesNoRace(t *testing.T) {
	t.Parallel()

	pipe := &alwaysRetryPipeline{}
	ag, _ := newPipelineTestAgent(t, pipe)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	proposal := sampleProposal()

	// Hammer the increment path — one call per iteration increments
	// the counter up to maxValidatorRetries, then surfaces.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			ag.runValidationPipeline(context.Background(), proposal)
		}
	}()

	// Hammer the delete path — recordEdit takes the lock and removes
	// the counter. Races with the increment goroutine's map access.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			ag.recordEdit(proposal)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	wg.Wait()

	// Sanity: the pipeline was actually invoked many times. Otherwise
	// the test passed trivially (no mutations → no race).
	if got := pipe.calls.Load(); got < 100 {
		t.Errorf("pipeline called %d times; test did not exercise enough iterations", got)
	}
}
