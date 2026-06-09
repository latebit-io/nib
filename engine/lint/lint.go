// Package lint defines the Linter interface and adapters that turn
// external lint tools into a structured signal the agent loop can act on.
//
// The core contract separates three states that any linter invocation can
// produce:
//
//  1. Infrastructure error — binary missing, config broken, timeout. The
//     linter itself failed; no meaningful findings are available.
//  2. Clean — the linter ran, zero findings.
//  3. Findings — the linter ran, N structured findings.
//
// Callers MUST distinguish all three. Treating "non-empty output" as
// "violations found" (the prior heuristic) collapses state 1 into state 3
// and sends the agent chasing phantom lint issues.
package lint

import "context"

// Finding is a single structured lint diagnostic.
// Line and Col are 1-indexed when reported; zero means unknown.
type Finding struct {
	// Path is the project-relative file path the finding applies to.
	Path string
	// Line is the 1-indexed source line; 0 when the linter did not report one.
	Line int
	// Col is the 1-indexed source column; 0 when the linter did not report one.
	Col int
	// Linter names the tool that produced the finding (e.g. "golangci-lint").
	Linter string
	// Message is the human-readable diagnostic, with any trailing rule/code
	// annotation appended in parens (e.g. "shadowed var (govet)").
	Message string
}

// Result is the outcome of a single Linter.Run invocation.
//
// Error is non-nil only when the linter itself failed to produce a usable
// signal (binary not on PATH, timeout, parse error on structured output,
// config file rejected). A linter that ran successfully and reported
// violations has Findings populated and Error nil.
type Result struct {
	// Findings holds all diagnostics the linter produced, in the order it
	// emitted them. Callers filter by path when splitting edited-file
	// findings from sibling-file findings.
	Findings []Finding
	// Error is set when the linter failed to run or produced output the
	// adapter could not parse. Never use a non-nil Error as a proxy for
	// "found violations" — the two states are distinct.
	Error error
}

// Linter runs an external lint tool against a package directory and
// returns the structured result. Implementations must:
//
//   - Return Result{Error: ...} on infrastructure failure (missing binary,
//     timeout, unparseable output). Do not fabricate findings from stderr.
//   - Return Result{Findings: nil} on clean runs. Do not return a sentinel
//     "no issues" Finding.
//   - Populate Finding.Path with project-relative paths when possible so
//     callers can filter reliably without string heuristics.
type Linter interface {
	// Name is the adapter identifier, used in Finding.Linter and in banner
	// text ("Style lint: golangci-lint failed — ..."). Stable across runs.
	Name() string
	// Run executes the linter against dir (project-relative package path)
	// rooted at projectRoot. files lists the project-relative paths of the
	// specific files the agent edited within dir — package-oriented
	// adapters (golangci-lint, go vet) ignore it; per-file adapters
	// (luacheck, raw) iterate it. ctx governs cancellation and timeout.
	Run(ctx context.Context, projectRoot, dir string, files []string) Result
}
