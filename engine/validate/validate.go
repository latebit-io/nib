// Package validate defines the port for pre-approval checks against
// proposed edits. Validators inspect a candidate (the expected post-edit
// file content) and return a structured verdict: pass, retry with
// feedback, or block for human review.
//
// The engine core depends only on the interfaces in this file. Concrete
// adapters (go/parser, tree-sitter, LSP shadow-buffer, go vet) live in
// sub-packages and are composed into a [Pipeline] at the composition root.
// A [NoopPipeline] satisfies the null-object pattern so the agent's
// proposal-handling path can dispatch unconditionally.
package validate

import (
	"context"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// LanguageFunc returns the tree-sitter grammar for a given path, or nil
// when the extension is unsupported. Defined here so every grammar-aware
// validator (treesitter, architecture, future per-language stages)
// shares one source of truth — composition roots can resolve a single
// [highlight.LanguageFor] reference and pass it to N validators without
// each subpackage re-declaring the contract.
//
// The returned [*sitter.Language] is expected to be cached and reused;
// validators must not call Close() on it.
type LanguageFunc func(path string) *sitter.Language

// Candidate describes a proposed edit awaiting validation. Before and
// After hold full file contents so stateless validators (parser,
// tree-sitter) can operate without side effects; LSP-backed validators
// that need a document URI can reconstruct one from Path.
type Candidate struct {
	// Path is the project-relative path of the file being edited.
	Path string

	// CanonPath is the canonical absolute path, matching the session's
	// workspace addressing scheme.
	CanonPath string

	// Before is the full file content before the edit is applied.
	Before string

	// After is the full file content that would exist if the edit were
	// applied as proposed. Validators analyse After, not the edit
	// fragment, so line-number feedback is globally accurate.
	After string
}

// Verdict describes a validator's overall judgement on a candidate.
type Verdict int

const (
	// Pass means the candidate is acceptable for this validator. The
	// pipeline continues evaluating remaining validators.
	Pass Verdict = iota

	// Retry means the candidate is unacceptable but the LLM can likely
	// fix it with structured feedback. The pipeline stops, the agent
	// loop sends the accumulated Feedback back to the model, and the
	// proposal is regenerated — without bothering the developer.
	Retry
)

// String returns a stable human-readable label for the verdict — used in
// capture payloads, debug logs, and test output.
func (v Verdict) String() string {
	switch v {
	case Pass:
		return "pass"
	case Retry:
		return "retry"
	default:
		return "unknown"
	}
}

// Result is one validator's findings for one candidate. Stage identifies
// the validator so the agent can attribute feedback and the capture sink
// can group per-stage outcomes in the session log.
type Result struct {
	// Verdict is the validator's judgement for this candidate.
	Verdict Verdict

	// Stage is a stable identifier for the validator (e.g. "go-parse",
	// "tree-sitter", "lsp-shadow", "go-vet"). Matches Validator.Name.
	Stage string

	// Feedback is the LLM-facing retry prompt. Non-empty on Retry;
	// empty on Pass.
	Feedback string
}

// Validator analyses a Candidate and returns a single Result. Validators
// must be stateless with respect to the Candidate (the same input yields
// the same verdict) so the pipeline can be run concurrently or cached in
// the future without semantic drift.
type Validator interface {
	// Name returns a stable identifier reused as Result.Stage. Matches
	// the lint.Linter.Name convention so capture payloads can reference
	// both without ambiguity.
	Name() string

	// Applicable reports whether this validator should run for the
	// given candidate (fast check — file extension, presence of a
	// language server, etc.). Returning false skips the validator
	// without producing a Result.
	Applicable(c Candidate) bool

	// Validate runs the check. Implementations must honour ctx.Done()
	// so an agent cancellation can abort an in-flight validation.
	Validate(ctx context.Context, c Candidate) Result
}

// Pipeline runs a set of validators against a candidate and returns
// their collected results. The engine core depends on this interface
// only; concrete implementations are supplied at the composition root.
//
// Implementations decide whether to short-circuit on the first non-Pass
// verdict (the default sequential pipeline does) or to run every
// applicable validator unconditionally (useful for audit modes).
type Pipeline interface {
	// Run evaluates the candidate and returns one Result per validator
	// that actually ran (skipped validators produce no Result). An
	// empty slice means all validators passed or none were applicable.
	Run(ctx context.Context, c Candidate) []Result
}

// NoopPipeline is the null-object implementation of Pipeline. It is the
// default installed by [github.com/latebit-io/nib/coding/agent.New]
// when no validators are registered; swap it out at the composition
// root to enable pre-approval checks.
type NoopPipeline struct{}

// Run returns a nil slice — every candidate passes.
func (NoopPipeline) Run(context.Context, Candidate) []Result { return nil }

// NewPipeline returns a sequential Pipeline that evaluates validators
// in order and short-circuits on the first non-Pass verdict. Passing
// zero validators yields a [NoopPipeline] so the common "no stages
// registered" path is the null object without special-casing.
//
// The returned pipeline respects ctx.Done() between validators and
// propagates cancellation into each Validator.Validate call.
func NewPipeline(validators ...Validator) Pipeline {
	if len(validators) == 0 {
		return NoopPipeline{}
	}
	return &seqPipeline{validators: validators}
}

// seqPipeline is the default sequential Pipeline implementation.
// Unexported — callers construct it via NewPipeline so future
// implementations (parallel, audit-all, cached) remain drop-in.
type seqPipeline struct {
	validators []Validator
}

// Run evaluates each applicable validator in registration order and
// stops at the first non-Pass verdict so the agent sees the
// highest-priority failure unambiguously.
func (p *seqPipeline) Run(ctx context.Context, c Candidate) []Result {
	results := make([]Result, 0, len(p.validators))
	for _, v := range p.validators {
		if ctx.Err() != nil {
			return results
		}
		if !v.Applicable(c) {
			continue
		}
		r := v.Validate(ctx, c)
		results = append(results, r)
		if r.Verdict != Pass {
			return results
		}
	}
	return results
}

// AnyNonPass reports whether any result carries a non-Pass verdict.
// Used by agent orchestration to decide between silent retry and
// pass-through. An empty slice yields false.
func AnyNonPass(results []Result) bool {
	for _, r := range results {
		if r.Verdict != Pass {
			return true
		}
	}
	return false
}
