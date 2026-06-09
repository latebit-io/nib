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

	"github.com/latebit-io/nib/engine/validate"
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

	return validate.Result{
		Verdict:  validate.Retry,
		Stage:    StageName,
		Feedback: formatFeedback(c.Path, err),
	}
}

// formatFeedback renders a parser error (possibly a scanner.ErrorList)
// into a single retry prompt the LLM can act on. Includes line numbers
// so the model can locate the fault without rereading the whole file.
func formatFeedback(path string, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your proposed edit to %s does not parse as valid Go. ", path)
	b.WriteString("Fix the syntax error(s) below and propose the edit again.\n\n")
	if list, ok := err.(scanner.ErrorList); ok {
		for _, e := range list {
			fmt.Fprintf(&b, "  - line %d:%d — %s\n", e.Pos.Line, e.Pos.Column, e.Msg)
		}
	} else {
		fmt.Fprintf(&b, "  - %s\n", err.Error())
	}
	return b.String()
}
