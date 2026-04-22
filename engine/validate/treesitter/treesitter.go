// Package treesitter implements a validate.Validator that parses the
// candidate's before/after content with tree-sitter and flags edits that
// introduce new syntax errors.
//
// The validator is language-agnostic: it takes a [LanguageFunc] at
// construction and routes each candidate to the matching grammar.
// Composition roots typically wire it to [highlight.LanguageFor] so the
// same grammar registry used for syntax highlighting also gates
// pre-approval validation — but the dependency is one-way and optional,
// so this package does not import engine/highlight.
//
// Semantics: tree-sitter always produces a tree, even for severely broken
// input, by inserting ERROR and MISSING nodes. Comparing the error count
// of Before vs After isolates regressions: an edit is flagged only when
// it strictly increases the error population. A file that started broken
// stays valid-to-edit until the edit makes it worse.
package treesitter

import (
	"context"
	"fmt"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/latebit-io/junto/engine/lint"
	"github.com/latebit-io/junto/engine/validate"
)

// StageName identifies this validator in Result.Stage and capture payloads.
const StageName = "tree-sitter"

// maxReportedErrors caps how many error-node positions appear in the
// retry feedback so a massively broken edit does not produce a prompt
// so large it blows out the model's context window.
const maxReportedErrors = 5

// LanguageFunc returns the tree-sitter grammar for a given path, or nil
// when the extension is unsupported. The returned *sitter.Language is
// expected to be cached and reused by the caller — the validator does
// not call Close() on it.
type LanguageFunc func(path string) *sitter.Language

// Validator is the tree-sitter-backed syntax-regression validator.
type Validator struct {
	langFor LanguageFunc
}

// New constructs a Validator wired to the given language resolver.
// A nil langFor makes the validator a permanent no-op — Applicable
// returns false, Validate returns Pass. This matches the overall
// null-object posture: missing dependencies degrade to pass-through.
func New(langFor LanguageFunc) *Validator {
	return &Validator{langFor: langFor}
}

// Name returns the stable stage identifier.
func (Validator) Name() string { return StageName }

// Applicable reports whether a grammar is registered for the candidate's
// path extension. Returns false when the LanguageFunc is nil or returns
// nil, so the pipeline skips tree-sitter entirely for unsupported files.
func (v *Validator) Applicable(c validate.Candidate) bool {
	if v.langFor == nil {
		return false
	}
	return v.langFor(c.Path) != nil
}

// Validate parses Before and After with the language grammar, counts
// error nodes in each tree, and flags the edit when After's count
// exceeds Before's. The returned Feedback embeds up to maxReportedErrors
// positions so the LLM can locate the new faults.
func (v *Validator) Validate(ctx context.Context, c validate.Candidate) validate.Result {
	if v.langFor == nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}
	lang := v.langFor(c.Path)
	if lang == nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	beforeErrors, err := countErrors(ctx, lang, c.Before)
	if err != nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}
	afterErrors, err := collectErrors(ctx, lang, c.After)
	if err != nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	if len(afterErrors) <= beforeErrors {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	findings := findingsFromErrors(c.Path, afterErrors, maxReportedErrors)
	return validate.Result{
		Verdict:  validate.Retry,
		Stage:    StageName,
		Findings: findings,
		Feedback: formatFeedback(c.Path, findings, len(afterErrors)-beforeErrors),
	}
}

// errorPos captures an error node's position for reporting.
type errorPos struct {
	Row, Col uint
	Missing  bool
}

// countErrors parses src and returns just the error-node total. Cheaper
// than collectErrors because it discards positions; used for the Before
// baseline where positions do not need to be reported.
func countErrors(ctx context.Context, lang *sitter.Language, src string) (int, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return 0, fmt.Errorf("tree-sitter: SetLanguage: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tree := parser.Parse([]byte(src), nil)
	if tree == nil {
		return 0, fmt.Errorf("tree-sitter: parse returned nil")
	}
	defer tree.Close()

	root := tree.RootNode()
	if !root.HasError() {
		return 0, nil
	}
	var count int
	visitErrors(root, func(*sitter.Node) bool {
		count++
		return true
	})
	return count, nil
}

// collectErrors parses src and returns the position of every error node
// in document order. Used for the After tree so the feedback string can
// include line numbers for the regressions.
func collectErrors(ctx context.Context, lang *sitter.Language, src string) ([]errorPos, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return nil, fmt.Errorf("tree-sitter: SetLanguage: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tree := parser.Parse([]byte(src), nil)
	if tree == nil {
		return nil, fmt.Errorf("tree-sitter: parse returned nil")
	}
	defer tree.Close()

	root := tree.RootNode()
	if !root.HasError() {
		return nil, nil
	}
	var out []errorPos
	visitErrors(root, func(n *sitter.Node) bool {
		p := n.StartPosition()
		out = append(out, errorPos{Row: p.Row, Col: p.Column, Missing: n.IsMissing()})
		return true
	})
	return out, nil
}

// visitErrors walks n and its descendants, invoking visit for each node
// where IsError() or IsMissing() is true. Prunes subtrees with no error
// markers to keep the walk proportional to the number of faults.
func visitErrors(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil || !n.HasError() {
		return
	}
	if n.IsError() || n.IsMissing() {
		if !visit(n) {
			return
		}
	}
	for i := range n.ChildCount() {
		visitErrors(n.Child(i), visit)
	}
}

// findingsFromErrors converts up to limit error positions into
// lint.Finding values for structured reporting.
func findingsFromErrors(path string, errs []errorPos, limit int) []lint.Finding {
	if limit <= 0 || len(errs) < limit {
		limit = len(errs)
	}
	out := make([]lint.Finding, 0, limit)
	for _, e := range errs[:limit] {
		msg := "unexpected syntax"
		if e.Missing {
			msg = "missing required syntax token"
		}
		out = append(out, lint.Finding{
			Path:    path,
			Line:    int(e.Row) + 1, // tree-sitter rows are 0-indexed
			Col:     int(e.Col) + 1,
			Linter:  StageName,
			Message: msg,
		})
	}
	return out
}

// formatFeedback renders the findings and regression count into a
// prompt the LLM can act on.
func formatFeedback(path string, findings []lint.Finding, regressions int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your edit to %s introduced %d new syntax error(s) ", path, regressions)
	b.WriteString("that tree-sitter flagged. Fix the issues below and propose the edit again.\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  - line %d:%d — %s\n", f.Line, f.Col, f.Message)
	}
	if regressions > len(findings) {
		fmt.Fprintf(&b, "  … and %d more\n", regressions-len(findings))
	}
	return b.String()
}
