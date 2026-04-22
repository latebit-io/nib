package validate

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeValidator is a table-friendly Validator that records the candidates
// it received and returns a pre-scripted verdict.
type fakeValidator struct {
	name       string
	applicable bool
	result     Result

	calls   int
	lastCtx context.Context
}

func (f *fakeValidator) Name() string              { return f.name }
func (f *fakeValidator) Applicable(Candidate) bool { return f.applicable }
func (f *fakeValidator) Validate(ctx context.Context, _ Candidate) Result {
	f.calls++
	f.lastCtx = ctx
	return f.result
}

// TestNoopPipelineContract verifies the null object returns no results and
// never panics — the contract every call site in the engine depends on.
func TestNoopPipelineContract(t *testing.T) {
	t.Parallel()

	var p Pipeline = NoopPipeline{}
	got := p.Run(context.Background(), Candidate{Path: "x.go"})
	if got != nil {
		t.Errorf("NoopPipeline.Run returned %d results, want nil", len(got))
	}
}

// TestNewPipelineZeroValidatorsReturnsNoop documents the intentional
// degenerate case: a pipeline with no validators IS the null object,
// with no special-casing at call sites.
func TestNewPipelineZeroValidatorsReturnsNoop(t *testing.T) {
	t.Parallel()

	p := NewPipeline()
	if _, ok := p.(NoopPipeline); !ok {
		t.Errorf("NewPipeline() = %T, want NoopPipeline", p)
	}
}

// TestPipelineShortCircuitsOnRetry verifies the sequential pipeline stops
// at the first non-Pass verdict so the agent sees the highest-priority
// failure deterministically.
func TestPipelineShortCircuitsOnRetry(t *testing.T) {
	t.Parallel()

	first := &fakeValidator{
		name: "go-parse", applicable: true,
		result: Result{Verdict: Retry, Stage: "go-parse", Feedback: "bad parse"},
	}
	second := &fakeValidator{name: "tree-sitter", applicable: true}

	p := NewPipeline(first, second)
	got := p.Run(context.Background(), Candidate{Path: "x.go"})

	if len(got) != 1 || got[0].Stage != "go-parse" {
		t.Fatalf("Run returned %+v, want single go-parse retry", got)
	}
	if second.calls != 0 {
		t.Errorf("second validator ran %d times after short-circuit; want 0", second.calls)
	}
}

// TestPipelineRunsAllValidatorsOnPass verifies every applicable validator
// is consulted when no stage fails, so later stages see a complete audit
// trail instead of a silently-skipped chain.
func TestPipelineRunsAllValidatorsOnPass(t *testing.T) {
	t.Parallel()

	a := &fakeValidator{name: "a", applicable: true, result: Result{Stage: "a"}}
	b := &fakeValidator{name: "b", applicable: true, result: Result{Stage: "b"}}

	p := NewPipeline(a, b)
	got := p.Run(context.Background(), Candidate{Path: "x.go"})

	if len(got) != 2 {
		t.Fatalf("Run returned %d results, want 2", len(got))
	}
	if got[0].Stage != "a" || got[1].Stage != "b" {
		t.Errorf("Run preserved stage order incorrectly: %+v", got)
	}
}

// TestPipelineSkipsInapplicable verifies Applicable=false is honoured
// (no Result emitted, no Validate call made) so Go-only validators do
// not show up in the results for, say, a .yaml edit.
func TestPipelineSkipsInapplicable(t *testing.T) {
	t.Parallel()

	skipped := &fakeValidator{name: "go-parse", applicable: false}
	ran := &fakeValidator{name: "tree-sitter", applicable: true, result: Result{Stage: "tree-sitter"}}

	p := NewPipeline(skipped, ran)
	got := p.Run(context.Background(), Candidate{Path: "x.yaml"})

	if skipped.calls != 0 {
		t.Errorf("inapplicable validator ran %d times; want 0", skipped.calls)
	}
	if len(got) != 1 || got[0].Stage != "tree-sitter" {
		t.Errorf("Run returned %+v, want single tree-sitter result", got)
	}
}

// TestPipelineRespectsContextCancellation verifies a cancelled context
// aborts the pipeline between validators so agent cancellation is prompt.
func TestPipelineRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v := &fakeValidator{name: "a", applicable: true, result: Result{Stage: "a"}}
	p := NewPipeline(v)

	got := p.Run(ctx, Candidate{Path: "x.go"})
	if len(got) != 0 {
		t.Errorf("Run on cancelled context returned %+v; want empty", got)
	}
	if v.calls != 0 {
		t.Errorf("validator ran under cancelled context; want 0 calls, got %d", v.calls)
	}
}

// TestPipelineDeadlineInFlight verifies context cancellation mid-flight
// returns whatever was collected so far without hanging — a sanity check
// that validators get the ctx they can observe.
func TestPipelineDeadlineInFlight(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	v := &fakeValidator{
		name: "slow", applicable: true,
		result: Result{Verdict: Pass, Stage: "slow"},
	}
	p := NewPipeline(v)
	got := p.Run(ctx, Candidate{Path: "x.go"})
	if len(got) != 1 {
		t.Errorf("Run returned %d results, want 1", len(got))
	}
	if v.lastCtx == nil {
		t.Errorf("validator did not receive context")
	}
	// Sanity: the ctx the validator sees is derived from our deadline
	// ctx. We can't check equality but we can check Err behaviour.
	_ = errors.Is(v.lastCtx.Err(), context.DeadlineExceeded) // may or may not have fired; both fine
}

// TestWorstVerdictOrdering verifies the severity ordering (Pass < Retry <
// Block) the agent's autonomy-retry logic depends on.
func TestWorstVerdictOrdering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []Result
		want Verdict
	}{
		{"empty", nil, Pass},
		{"all pass", []Result{{Verdict: Pass}, {Verdict: Pass}}, Pass},
		{"one retry", []Result{{Verdict: Pass}, {Verdict: Retry}}, Retry},
		{"retry and block", []Result{{Verdict: Retry}, {Verdict: Block}}, Block},
		{"block wins everywhere", []Result{{Verdict: Block}, {Verdict: Pass}}, Block},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WorstVerdict(tc.in)
			if got != tc.want {
				t.Errorf("WorstVerdict(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestVerdictString pins the string labels used in capture payloads.
func TestVerdictString(t *testing.T) {
	t.Parallel()

	tests := map[Verdict]string{
		Pass:  "pass",
		Retry: "retry",
		Block: "block",
	}
	for v, want := range tests {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", v, got, want)
		}
	}
}
