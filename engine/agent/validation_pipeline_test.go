package agent

import (
	"context"
	"testing"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/validate"
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
