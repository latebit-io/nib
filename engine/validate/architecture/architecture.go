// Package architecture implements a [validate.Validator] that enforces
// deterministic structural caps on proposed edits — total file lines,
// per-function lines, and number of functions per file. The caps are
// owned by the active coding style ([styleconfig.Architecture]) so a
// developer who picks "clean-code" gets stricter caps than one who picks
// "idiomatic-go" without the validator package needing to know which.
//
// Function bounds are detected via tree-sitter when a grammar is
// registered for the file's extension. Files in unsupported languages
// are exempt entirely — the validator's [Validator.Applicable] returns
// false so config files, docs, and unmapped languages do not produce
// false positives.
package architecture

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/latebit-io/nib/engine/lint"
	"github.com/latebit-io/nib/engine/styleconfig"
	"github.com/latebit-io/nib/engine/validate"
)

// StageName identifies this validator in [validate.Result.Stage] and
// capture payloads.
const StageName = "architecture"

// maxReportedFunctions caps how many oversized-function findings appear
// in a single retry feedback string so a wholesale rewrite does not
// produce a prompt large enough to blow out the model's context window.
const maxReportedFunctions = 5

// Provider is the interface the validator depends on for resolving the
// active architecture caps. The styleconfig package's *ActiveProvider
// satisfies it; tests can supply a stub.
type Provider interface {
	// Architecture returns a snapshot of the active configuration.
	Architecture() styleconfig.Architecture
}

// Validator is the architecture-cap pre-approval validator.
type Validator struct {
	provider Provider
	langFor  validate.LanguageFunc
}

// New constructs a Validator wired to the given architecture provider
// and language resolver. A nil provider or langFor makes the validator
// a no-op — Applicable returns false, Validate returns Pass.
func New(provider Provider, langFor validate.LanguageFunc) *Validator {
	return &Validator{provider: provider, langFor: langFor}
}

// Name returns the stable stage identifier.
func (Validator) Name() string { return StageName }

// Applicable reports whether the validator should run for c. The check
// gates on three conditions: a provider is registered, the active
// configuration has at least one cap enabled, and the file's language
// is registered with the grammar resolver. Files in unsupported
// languages (configuration, documentation, untagged extensions) skip
// architecture checks entirely.
func (v *Validator) Applicable(c validate.Candidate) bool {
	if v.provider == nil {
		return false
	}
	if !v.provider.Architecture().Enabled() {
		return false
	}
	if v.langFor == nil || v.langFor(c.Path) == nil {
		return false
	}
	return true
}

// Validate counts lines, parses function bounds via tree-sitter, and
// reports cap violations. Verdict is Block when the active configuration
// sets Action="block" and any cap is exceeded; Retry otherwise. The
// architecture snapshot is taken once at the start of the call to
// avoid double-read races against a concurrent style cycle.
//
// Honours the nil-safety contract documented on [New]: a Validator
// constructed with nil provider or nil langFor returns Pass. The
// pipeline never invokes Validate on a non-Applicable candidate, but
// direct callers (tests, custom orchestration) may bypass that
// pre-check, so the guards live here too.
func (v *Validator) Validate(ctx context.Context, c validate.Candidate) validate.Result {
	if v.provider == nil || v.langFor == nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}
	lang := v.langFor(c.Path)
	if lang == nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}
	arch := v.provider.Architecture()
	if !arch.Enabled() {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	var findings []lint.Finding
	var feedback strings.Builder

	if arch.MaxFileLines > 0 {
		afterLines := countLines(c.After)
		if afterLines > arch.MaxFileLines {
			findings = append(findings, lint.Finding{
				Path:    c.Path,
				Line:    afterLines,
				Linter:  StageName,
				Message: fmt.Sprintf("file is %d lines (max %d)", afterLines, arch.MaxFileLines),
			})
			fmt.Fprintf(&feedback,
				"%s would be %d lines after this edit; the active style caps files at %d. "+
					"Split this file into smaller modules grouped by responsibility "+
					"(input, render, state, audio, etc.) before proposing the edit again.\n",
				c.Path, afterLines, arch.MaxFileLines)
		}
	}

	if arch.MaxFunctionLines > 0 || arch.MaxFunctionsPerFile > 0 {
		spans, err := parseFunctionSpans(ctx, lang, c.After)
		switch {
		case err != nil:
			// Parser failures (tree-sitter setup, broken grammar,
			// ctx cancellation mid-parse) are infrastructure
			// noise, not a code-quality signal the LLM can act
			// on — surfacing as a finding would mislead. Log
			// loudly via slog.Warn so the developer can see
			// when function-cap checks degrade silently, then
			// continue with whatever the file-line cap path
			// already produced.
			slog.Warn("architecture: function-cap parse failed",
				"path", c.Path, "err", err)
		default:
			fnFindings, fnFeedback := checkFunctionCaps(c.Path, spans, arch)
			findings = append(findings, fnFindings...)
			if fnFeedback != "" {
				if feedback.Len() > 0 {
					feedback.WriteByte('\n')
				}
				feedback.WriteString(fnFeedback)
			}
		}
	}

	if len(findings) == 0 {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	verdict := validate.Retry
	if arch.IsBlock() {
		verdict = validate.Block
	}
	return validate.Result{
		Verdict:  verdict,
		Stage:    StageName,
		Findings: findings,
		Feedback: feedback.String(),
	}
}

// countLines counts newline-delimited lines in src. An empty string is
// 0 lines. A string ending without a trailing newline still counts the
// final line — matching wc -l semantics for "lines of content."
func countLines(src string) int {
	if src == "" {
		return 0
	}
	n := strings.Count(src, "\n")
	if !strings.HasSuffix(src, "\n") {
		n++
	}
	return n
}

// fnSpan describes one function or method declaration found in the
// candidate's After content.
type fnSpan struct {
	StartLine int // 1-indexed
	EndLine   int // 1-indexed
}

// Lines returns the number of lines covered by the span, inclusive.
func (s fnSpan) Lines() int {
	return s.EndLine - s.StartLine + 1
}

// parseFunctionSpans walks a tree-sitter parse tree and returns spans
// for every function-like declaration. Function-like is detected
// heuristically by node type name: any type containing "function" or
// "method" qualifies. This covers Go (function_declaration,
// method_declaration, func_literal), Lua (function_declaration,
// function_definition, local_function), JavaScript/TypeScript, Python,
// Rust, and most other tree-sitter grammars without per-language
// special cases.
func parseFunctionSpans(ctx context.Context, lang *sitter.Language, src string) ([]fnSpan, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return nil, fmt.Errorf("architecture: SetLanguage: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tree := parser.Parse([]byte(src), nil)
	if tree == nil {
		return nil, fmt.Errorf("architecture: parse returned nil")
	}
	defer tree.Close()

	var spans []fnSpan
	visitFunctions(tree.RootNode(), func(n *sitter.Node) {
		start := n.StartPosition()
		end := n.EndPosition()
		spans = append(spans, fnSpan{
			StartLine: int(start.Row) + 1,
			EndLine:   int(end.Row) + 1,
		})
	})
	return spans, nil
}

// functionLikeKinds is the explicit allow-list of tree-sitter node type
// names that count as function or method declarations. Drawn from the
// grammars we support today (Go, Lua) plus common shapes for
// languages we are likely to grow into (JS/TS, Python, Rust). A
// substring match like "contains function" was tried first but
// double-counted Lua `function_name` child nodes inside
// `function_declaration` parents — explicit names are deterministic.
var functionLikeKinds = map[string]bool{
	// Go
	"function_declaration": true, // also matches Lua, Python, JS/TS
	"method_declaration":   true,
	"func_literal":         true,

	// Lua
	"local_function":      true,
	"function_definition": true, // anonymous function; also Python, C/C++

	// JavaScript / TypeScript
	"function_expression": true,
	"arrow_function":      true,
	"method_definition":   true,
	"generator_function":  true,

	// Rust
	"function_item": true,
}

// visitFunctions walks n and its descendants, invoking visit on every
// node whose Kind() is in [functionLikeKinds]. Nested functions
// (closures, lambdas, methods declared inside class bodies) are
// reported separately because each one is its own match.
func visitFunctions(n *sitter.Node, visit func(*sitter.Node)) {
	if n == nil {
		return
	}
	if functionLikeKinds[n.Kind()] {
		visit(n)
	}
	for i := range n.ChildCount() {
		visitFunctions(n.Child(i), visit)
	}
}

// checkFunctionCaps applies the per-function and per-file function-count
// caps to spans. Returns lint findings and a single concatenated
// feedback string. An empty findings slice means no caps were exceeded.
func checkFunctionCaps(path string, spans []fnSpan, arch styleconfig.Architecture) ([]lint.Finding, string) {
	var findings []lint.Finding
	var feedback strings.Builder

	if arch.MaxFunctionLines > 0 {
		var oversized []fnSpan
		for _, s := range spans {
			if s.Lines() > arch.MaxFunctionLines {
				oversized = append(oversized, s)
			}
		}
		if len(oversized) > 0 {
			limit := min(maxReportedFunctions, len(oversized))
			for _, s := range oversized[:limit] {
				findings = append(findings, lint.Finding{
					Path:    path,
					Line:    s.StartLine,
					Linter:  StageName,
					Message: fmt.Sprintf("function spans %d lines (max %d)", s.Lines(), arch.MaxFunctionLines),
				})
			}
			fmt.Fprintf(&feedback,
				"%s contains %d function(s) over the %d-line cap. "+
					"Extract helpers so each function does one thing — review the bodies starting at:\n",
				path, len(oversized), arch.MaxFunctionLines)
			for _, s := range oversized[:limit] {
				fmt.Fprintf(&feedback, "  - line %d (%d lines)\n", s.StartLine, s.Lines())
			}
			if len(oversized) > limit {
				fmt.Fprintf(&feedback, "  … and %d more\n", len(oversized)-limit)
			}
		}
	}

	if arch.MaxFunctionsPerFile > 0 && len(spans) > arch.MaxFunctionsPerFile {
		findings = append(findings, lint.Finding{
			Path:    path,
			Line:    1,
			Linter:  StageName,
			Message: fmt.Sprintf("file declares %d functions (max %d)", len(spans), arch.MaxFunctionsPerFile),
		})
		if feedback.Len() > 0 {
			feedback.WriteByte('\n')
		}
		fmt.Fprintf(&feedback,
			"%s declares %d functions; the active style caps files at %d. "+
				"Move related functions into a sibling file or package by responsibility.\n",
			path, len(spans), arch.MaxFunctionsPerFile)
	}

	return findings, feedback.String()
}
