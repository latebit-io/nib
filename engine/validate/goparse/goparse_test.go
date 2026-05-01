package goparse

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/engine/validate"
)

// TestApplicable pins the file-extension gate. Non-Go files must be
// skipped so the validator never tries to parse, say, YAML.
func TestApplicable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"engine/session/session.go", true},
		{"main.yaml", false},
		{"go.mod", false},
		{"README.md", false},
		{"", false},
	}
	var v Validator
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := v.Applicable(validate.Candidate{Path: tc.path})
			if got != tc.want {
				t.Errorf("Applicable(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestValidateCases covers the parser's behaviour against representative
// valid and broken inputs. Each broken case asserts that (a) the verdict
// is Retry, (b) findings reference the expected line, and (c) the
// feedback string carries enough context for the LLM to locate the fault.
//
//nolint:gocognit // table-driven test — branch count reflects the cases, not logic
func TestValidateCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		after       string
		wantVerdict validate.Verdict
		wantLine    int    // 0 = don't check
		wantMsgSub  string // substring must appear in the first finding's message
	}{
		{
			name:        "valid go",
			after:       "package main\n\nfunc main() {}\n",
			wantVerdict: validate.Pass,
		},
		{
			name:        "missing closing brace",
			after:       "package main\n\nfunc main() {\n",
			wantVerdict: validate.Retry,
			wantLine:    3, // parser points at the unmatched `{`, not EOF
			wantMsgSub:  "}",
		},
		{
			name:        "bad import",
			after:       "package main\n\nimport \"unterminated\n",
			wantVerdict: validate.Retry,
			wantLine:    3,
		},
		{
			name:        "no package clause",
			after:       "func main() {}\n",
			wantVerdict: validate.Retry,
			wantLine:    1,
			wantMsgSub:  "package",
		},
		{
			name:        "doc comment only is still valid",
			after:       "// Package x does things.\npackage x\n",
			wantVerdict: validate.Pass,
		},
	}

	var v Validator
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := v.Validate(context.Background(), validate.Candidate{
				Path:  "test.go",
				After: tc.after,
			})
			if got.Verdict != tc.wantVerdict {
				t.Fatalf("Verdict = %v, want %v (feedback=%q)", got.Verdict, tc.wantVerdict, got.Feedback)
			}
			if got.Stage != StageName {
				t.Errorf("Stage = %q, want %q", got.Stage, StageName)
			}
			if tc.wantVerdict == validate.Pass {
				if len(got.Findings) != 0 {
					t.Errorf("Pass case produced findings: %+v", got.Findings)
				}
				if got.Feedback != "" {
					t.Errorf("Pass case produced non-empty feedback: %q", got.Feedback)
				}
				return
			}
			if len(got.Findings) == 0 {
				t.Fatalf("Retry case produced no findings")
			}
			if tc.wantLine != 0 && got.Findings[0].Line != tc.wantLine {
				t.Errorf("first finding line = %d, want %d (msg=%q)",
					got.Findings[0].Line, tc.wantLine, got.Findings[0].Message)
			}
			if tc.wantMsgSub != "" && !strings.Contains(got.Findings[0].Message, tc.wantMsgSub) {
				t.Errorf("first finding message = %q, missing substring %q",
					got.Findings[0].Message, tc.wantMsgSub)
			}
			if got.Feedback == "" {
				t.Errorf("Retry case produced empty feedback")
			}
			if !strings.Contains(got.Feedback, "test.go") {
				t.Errorf("feedback missing file path: %q", got.Feedback)
			}
		})
	}
}

// TestFeedbackIncludesLineNumber verifies the feedback string embeds
// line numbers so the LLM can locate faults without re-reading the file.
func TestFeedbackIncludesLineNumber(t *testing.T) {
	t.Parallel()

	var v Validator
	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "x.go",
		After: "package main\n\nfunc main() {\n",
	})
	if got.Verdict != validate.Retry {
		t.Fatalf("Verdict = %v, want Retry", got.Verdict)
	}
	if !strings.Contains(got.Feedback, "line 3") {
		t.Errorf("feedback missing 'line 3': %q", got.Feedback)
	}
}
