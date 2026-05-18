package treesitter

import (
	"context"
	"strings"
	"testing"

	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/latebit-io/nib/engine/validate"
)

// luaLang returns a Lua grammar instance for tests. Cached per-process
// because tree-sitter language bindings are thread-safe and expensive to
// construct; tests exercise the validator against it repeatedly.
var luaLang = sitter.NewLanguage(tree_sitter_lua.Language())

// luaLangFor is a LanguageFunc that returns Lua for .lua files and nil
// otherwise, matching the pattern composition roots wire up in production.
func luaLangFor(path string) *sitter.Language {
	if strings.HasSuffix(path, ".lua") {
		return luaLang
	}
	return nil
}

// TestNilLanguageFuncIsNoOp verifies the null-object posture: a
// Validator with no resolver returns Pass and marks itself inapplicable
// everywhere, so misconfiguration degrades gracefully.
func TestNilLanguageFuncIsNoOp(t *testing.T) {
	t.Parallel()

	v := New(nil)
	c := validate.Candidate{Path: "x.lua", Before: "local x = 1", After: "local x = 1"}

	if v.Applicable(c) {
		t.Errorf("Applicable(nil langFor) = true, want false")
	}
	got := v.Validate(context.Background(), c)
	if got.Verdict != validate.Pass {
		t.Errorf("Validate(nil langFor) = %v, want Pass", got.Verdict)
	}
}

// TestApplicableGatedByLanguageFunc verifies the validator is skipped
// for any path the LanguageFunc does not recognise, so unsupported
// languages never reach the parser.
func TestApplicableGatedByLanguageFunc(t *testing.T) {
	t.Parallel()

	v := New(luaLangFor)
	tests := []struct {
		path string
		want bool
	}{
		{"script.lua", true},
		{"main.go", false},
		{"README.md", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := v.Applicable(validate.Candidate{Path: tc.path})
			if got != tc.want {
				t.Errorf("Applicable(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestValidatePassesWhenErrorsDoNotIncrease covers the key invariant:
// an edit that does not make the tree worse is accepted even if the
// tree started broken. Prevents blocking edits to already-broken files.
func TestValidatePassesWhenErrorsDoNotIncrease(t *testing.T) {
	t.Parallel()

	v := New(luaLangFor)

	tests := []struct {
		name          string
		before, after string
	}{
		{
			name:   "both clean",
			before: "local x = 1\nprint(x)\n",
			after:  "local x = 2\nprint(x)\n",
		},
		{
			name:   "already broken stays same",
			before: "local x = \n",
			after:  "local y = \n",
		},
		{
			name:   "fixes an error",
			before: "local x = \n",
			after:  "local x = 1\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := v.Validate(context.Background(), validate.Candidate{
				Path: "t.lua", Before: tc.before, After: tc.after,
			})
			if got.Verdict != validate.Pass {
				t.Errorf("Verdict = %v, want Pass (feedback=%q)", got.Verdict, got.Feedback)
			}
		})
	}
}

// TestValidateRetriesWhenEditRegresses covers the primary failure mode:
// an edit that introduces new errors must surface as Retry with a
// feedback string the LLM can act on.
func TestValidateRetriesWhenEditRegresses(t *testing.T) {
	t.Parallel()

	v := New(luaLangFor)
	got := v.Validate(context.Background(), validate.Candidate{
		Path:   "t.lua",
		Before: "local x = 1\n",
		After:  "local x = 1\n function f( end\n", // function signature is broken
	})

	if got.Verdict != validate.Retry {
		t.Fatalf("Verdict = %v, want Retry (feedback=%q)", got.Verdict, got.Feedback)
	}
	if got.Stage != StageName {
		t.Errorf("Stage = %q, want %q", got.Stage, StageName)
	}
	if got.Feedback == "" {
		t.Errorf("Retry with no feedback")
	}
	if !strings.Contains(got.Feedback, "t.lua") {
		t.Errorf("feedback missing path: %q", got.Feedback)
	}
	if !strings.Contains(got.Feedback, "line") {
		t.Errorf("feedback missing line reference: %q", got.Feedback)
	}
}

// TestUnsupportedExtensionPasses verifies the fallback path: when the
// LanguageFunc returns nil, Validate returns Pass even though the
// validator was reached (Applicable would normally prevent this, but
// test the defence-in-depth path).
func TestUnsupportedExtensionPasses(t *testing.T) {
	t.Parallel()

	v := New(luaLangFor)
	got := v.Validate(context.Background(), validate.Candidate{
		Path: "main.go", Before: "package main", After: "package main",
	})
	if got.Verdict != validate.Pass {
		t.Errorf("Verdict = %v for unsupported path, want Pass", got.Verdict)
	}
}

// TestCancelledContextPasses verifies ctx cancellation yields Pass
// rather than a spurious Retry — parse failures under cancellation
// must not be confused with real regressions.
func TestCancelledContextPasses(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v := New(luaLangFor)
	got := v.Validate(ctx, validate.Candidate{
		Path: "t.lua", Before: "local x = 1", After: "local x = \n",
	})
	if got.Verdict != validate.Pass {
		t.Errorf("cancelled ctx Verdict = %v, want Pass", got.Verdict)
	}
}

// TestFormatFeedbackCaps verifies the per-error cap in the rendered
// feedback, so the behaviour is tested deterministically without
// depending on what the grammar decides to collapse during error
// recovery.
func TestFormatFeedbackCaps(t *testing.T) {
	t.Parallel()

	errs := make([]errorPos, maxReportedErrors+3)
	for i := range errs {
		errs[i] = errorPos{Row: uint(i), Col: 0}
	}
	// Truncation hint fires when len(errs) > maxReportedErrors.
	feedback := formatFeedback("t.lua", errs, len(errs))
	if !strings.Contains(feedback, "more") {
		t.Errorf("feedback missing truncation hint: %q", feedback)
	}
}
