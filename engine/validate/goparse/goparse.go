// Package goparse implements a validate.Validator that runs the Go
// stdlib parser against a candidate's expected post-edit content. Parse
// errors yield a Retry verdict with line-numbered feedback so the LLM
// can self-correct without surfacing a broken proposal to the developer.
//
// This validator is intentionally stateless and cheap: a single call to
// parser.ParseFile over an in-memory string. No imports are resolved,
// no types are checked — semantic issues remain the LSP validator's job
// (future stage). The cost/value curve here is the best in the pipeline:
// the vast majority of hallucinated edits fail to parse, and catching
// them here saves an approval round-trip.
package goparse

import (
	"context"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"strings"

	"github.com/latebit-io/junto/engine/lint"
	"github.com/latebit-io/junto/engine/validate"
)

// StageName identifies this validator in Result.Stage and capture payloads.
const StageName = "go-parse"

// Validator is the Go-syntax validator. Zero-value is ready to use.
type Validator struct{}

// Name returns the stable stage identifier.
func (Validator) Name() string { return StageName }

// Applicable reports whether the candidate is a Go source file. Uses the
// path suffix rather than content sniffing because the parser would
// reject non-Go content outright anyway.
func (Validator) Applicable(c validate.Candidate) bool {
	return strings.HasSuffix(c.Path, ".go")
}

// Validate parses the candidate's After content and returns a Result.
// A successful parse yields Pass; any parse error yields Retry with a
// structured Feedback string the agent loop hands back to the LLM.
func (Validator) Validate(_ context.Context, c validate.Candidate) validate.Result {
	fset := token.NewFileSet()
	// Use ParseComments so doc-comment-only edits validate consistently;
	// the downside (retaining comments in the AST) is irrelevant because
	// the AST is immediately discarded.
	_, err := parser.ParseFile(fset, c.Path, c.After, parser.ParseComments)
	if err == nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	findings := errorsToFindings(c.Path, err)
	return validate.Result{
		Verdict:  validate.Retry,
		Stage:    StageName,
		Findings: findings,
		Feedback: formatFeedback(c.Path, findings),
	}
}

// errorsToFindings converts a parser error (possibly a scanner.ErrorList)
// into a stable slice of lint.Finding suitable for agent feedback and
// capture payloads.
func errorsToFindings(path string, err error) []lint.Finding {
	if list, ok := err.(scanner.ErrorList); ok {
		out := make([]lint.Finding, 0, len(list))
		for _, e := range list {
			out = append(out, lint.Finding{
				Path:    path,
				Line:    e.Pos.Line,
				Col:     e.Pos.Column,
				Linter:  StageName,
				Message: e.Msg,
			})
		}
		return out
	}
	return []lint.Finding{{
		Path:    path,
		Linter:  StageName,
		Message: err.Error(),
	}}
}

// formatFeedback renders findings into a single retry prompt the LLM
// can act on. Includes line numbers so the model can locate the fault
// without rereading the whole file.
func formatFeedback(path string, findings []lint.Finding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your proposed edit to %s does not parse as valid Go. ", path)
	b.WriteString("Fix the syntax error(s) below and propose the edit again.\n\n")
	for _, f := range findings {
		if f.Line > 0 {
			fmt.Fprintf(&b, "  - line %d:%d — %s\n", f.Line, f.Col, f.Message)
			continue
		}
		fmt.Fprintf(&b, "  - %s\n", f.Message)
	}
	return b.String()
}
