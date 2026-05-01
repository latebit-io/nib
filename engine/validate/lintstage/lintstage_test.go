package lintstage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/engine/lint"
	"github.com/latebit-io/nib/engine/validate"
)

// stubLinter is a controllable test double for [lint.Linter]. The
// captured arguments let tests assert that the candidate's content was
// actually written to a real file before the linter ran.
type stubLinter struct {
	name    string
	result  lint.Result
	runs    atomic.Int32
	lastDir string
	lastDB  []byte // bytes the linter saw on disk for the first file
}

func (s *stubLinter) Name() string { return s.name }

func (s *stubLinter) Run(_ context.Context, projectRoot, _ string, files []string) lint.Result {
	s.runs.Add(1)
	s.lastDir = projectRoot
	if len(files) > 0 {
		// Read the staged file so tests can verify content round-trip.
		full := filepath.Join(projectRoot, files[0])
		if data, err := os.ReadFile(full); err == nil {
			s.lastDB = data
		}
	}
	return s.result
}

// TestApplicableGates verifies the validator is inert when no source
// is configured, when the source returns no linters, or when the
// candidate has no extension (typical for binaries / scripts).
func TestApplicableGates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		source LinterSource
		path   string
		want   bool
	}{
		{
			name:   "nil source",
			source: nil,
			path:   "main.lua",
			want:   false,
		},
		{
			name:   "empty source",
			source: func() []lint.Linter { return nil },
			path:   "main.lua",
			want:   false,
		},
		{
			name:   "extensionless path",
			source: func() []lint.Linter { return []lint.Linter{&stubLinter{name: "x"}} },
			path:   "Makefile",
			want:   false,
		},
		{
			name:   "source with linter",
			source: func() []lint.Linter { return []lint.Linter{&stubLinter{name: "x"}} },
			path:   "main.lua",
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := New(tc.source)
			if got := v.Applicable(validate.Candidate{Path: tc.path}); got != tc.want {
				t.Errorf("Applicable(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestPassOnClean verifies a Pass verdict when the linter reports no
// findings and no error.
func TestPassOnClean(t *testing.T) {
	t.Parallel()

	stub := &stubLinter{name: "stub", result: lint.Result{}}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: "local x = 1\n",
	})
	if got.Verdict != validate.Pass {
		t.Errorf("Verdict = %v, want Pass", got.Verdict)
	}
	if got.Findings != nil {
		t.Errorf("Findings = %v, want nil", got.Findings)
	}
	if stub.runs.Load() != 1 {
		t.Errorf("linter ran %d times, want 1", stub.runs.Load())
	}
}

// TestRetryOnFindings verifies findings produce a Retry verdict with
// feedback that includes the path, count, line numbers, and linter tag
// so the LLM can act on the diagnostic.
func TestRetryOnFindings(t *testing.T) {
	t.Parallel()

	stub := &stubLinter{
		name: "stub",
		result: lint.Result{Findings: []lint.Finding{
			{Line: 5, Col: 12, Linter: "luacheck", Message: "unused variable 'foo'"},
			{Line: 9, Linter: "luacheck", Message: "global access"},
		}},
	}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: "local x = 1\n",
	})

	if got.Verdict != validate.Retry {
		t.Fatalf("Verdict = %v, want Retry; feedback=%q", got.Verdict, got.Feedback)
	}
	if !strings.Contains(got.Feedback, "main.lua") {
		t.Errorf("feedback missing real path: %q", got.Feedback)
	}
	if !strings.Contains(got.Feedback, "unused variable 'foo'") {
		t.Errorf("feedback missing first message: %q", got.Feedback)
	}
	if !strings.Contains(got.Feedback, "luacheck") {
		t.Errorf("feedback missing linter tag: %q", got.Feedback)
	}
}

// TestStagesContentToTempFile verifies the candidate's After content is
// actually written to a real file before the linter runs — proving the
// stage can be used with linters that read from disk (the entire point
// of writing a temp file in the first place).
func TestStagesContentToTempFile(t *testing.T) {
	t.Parallel()

	const content = "local hello = 'world'\nprint(hello)\n"
	stub := &stubLinter{name: "stub"}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	_ = v.Validate(context.Background(), validate.Candidate{
		Path:  "src/main.lua",
		After: content,
	})

	if got := string(stub.lastDB); got != content {
		t.Errorf("staged content = %q, want %q", got, content)
	}
	if !strings.Contains(stub.lastDir, brand.TempDirPrefix) {
		t.Errorf("staging dir %q does not contain prefix %q", stub.lastDir, brand.TempDirPrefix)
	}
}

// TestTempCleanup verifies the sandbox dir is removed after Validate
// returns. A leaked dir would accumulate across edits in long-running
// sessions.
func TestTempCleanup(t *testing.T) {
	t.Parallel()

	stub := &stubLinter{name: "stub"}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	_ = v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: "local x = 1\n",
	})

	if stub.lastDir == "" {
		t.Fatal("linter never ran; staging dir not captured")
	}
	if _, err := os.Stat(stub.lastDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging dir %q not cleaned up; err=%v", stub.lastDir, err)
	}
}

// TestPathsRewrittenToReal verifies every finding's Path is rewritten
// to the candidate's real path. The validator runs the linter on a
// single staged file in an isolated sandbox, so any finding emitted is
// necessarily about that file — rewriting unconditionally lets the LLM
// localise the issue without seeing /tmp/<brand.TempDirPrefix>… paths.
func TestPathsRewrittenToReal(t *testing.T) {
	t.Parallel()

	stub := &stubLinter{
		name: "stub",
		result: lint.Result{Findings: []lint.Finding{
			{Path: "candidate.lua", Line: 1, Message: "first"},
			{Path: "/tmp/" + brand.TempDirPrefix + "xyz/candidate.lua", Line: 2, Message: "absolute"},
			{Path: "", Line: 3, Message: "no path"},
			{Path: "/some/unrelated/path.lua", Line: 4, Message: "still gets rewritten"},
		}},
	}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "src/real.lua",
		After: "local x = 1\n",
	})

	if got.Verdict != validate.Retry {
		t.Fatalf("Verdict = %v, want Retry", got.Verdict)
	}
	for i, f := range got.Findings {
		if f.Path != "src/real.lua" {
			t.Errorf("finding[%d].Path = %q, want %q", i, f.Path, "src/real.lua")
		}
	}
}

// TestLinterErrorTreatedAsPass verifies an infrastructure error from a
// linter (missing binary, parse failure) does not produce Retry — the
// validator is permissive in the face of misconfiguration so the dev's
// flow is not blocked. Post-task review surfaces such errors instead.
func TestLinterErrorTreatedAsPass(t *testing.T) {
	t.Parallel()

	stub := &stubLinter{
		name:   "stub",
		result: lint.Result{Error: errors.New("luacheck: not on PATH")},
	}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: "local x = 1\n",
	})
	if got.Verdict != validate.Pass {
		t.Errorf("Verdict = %v, want Pass on infra error", got.Verdict)
	}
}

// TestNameStable locks the stage identifier in case downstream consumers
// match on it.
func TestNameStable(t *testing.T) {
	t.Parallel()

	if got := (Validator{}).Name(); got != StageName {
		t.Errorf("Name() = %q, want %q", got, StageName)
	}
}

// TestFindingsCappedAtMaxStored verifies a noisy linter (returning more
// than maxStoredFindings) cannot blow up Result.Findings. Without the
// cap, an 8 MiB output from luacheck on a generated file (~100k
// findings) would all sit in memory until GC'd. The cap means we keep
// the first 64 — enough for the 8-item display plus headroom — and
// drop the tail.
func TestFindingsCappedAtMaxStored(t *testing.T) {
	t.Parallel()

	noisy := make([]lint.Finding, maxStoredFindings*4)
	for i := range noisy {
		noisy[i] = lint.Finding{Line: i + 1, Linter: "noisy", Message: "x"}
	}
	stub := &stubLinter{name: "noisy", result: lint.Result{Findings: noisy}}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: "local x = 1\n",
	})

	if got.Verdict != validate.Retry {
		t.Fatalf("Verdict = %v, want Retry", got.Verdict)
	}
	if len(got.Findings) != maxStoredFindings {
		t.Errorf("len(Findings) = %d, want %d (cap)", len(got.Findings), maxStoredFindings)
	}
}

// TestFindingMessageClampedInFeedback verifies a single pathological
// diagnostic (multi-MB message) is truncated before reaching the
// retry feedback. Without this clamp, one bad finding would multiply
// the LLM's input-token bill.
func TestFindingMessageClampedInFeedback(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("X", maxFindingMessageBytes*4)
	stub := &stubLinter{
		name: "huge",
		result: lint.Result{Findings: []lint.Finding{
			{Line: 1, Linter: "huge", Message: huge},
		}},
	}
	v := New(func() []lint.Linter { return []lint.Linter{stub} })

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: "local x = 1\n",
	})

	if len(got.Feedback) >= len(huge) {
		t.Errorf("Feedback length %d >= huge message length %d; clamp not applied",
			len(got.Feedback), len(huge))
	}
	if !strings.Contains(got.Feedback, "(truncated)") {
		t.Errorf("Feedback missing truncation marker: %q", got.Feedback[:min(len(got.Feedback), 200)])
	}
}

// TestClampMessageBoundaries locks in the precise truncation behaviour
// so a future "let me make this rune-aware" or "let me drop the
// ellipsis" change has to update the test deliberately.
func TestClampMessageBoundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short", "hello", "hello"},
		{"exactly at cap", strings.Repeat("a", maxFindingMessageBytes), strings.Repeat("a", maxFindingMessageBytes)},
		{
			name: "over cap",
			in:   strings.Repeat("a", maxFindingMessageBytes+10),
			want: strings.Repeat("a", maxFindingMessageBytes-len(" …(truncated)")) + " …(truncated)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampMessage(tc.in)
			if got != tc.want {
				t.Errorf("clampMessage(len=%d) = %q (len=%d), want length %d",
					len(tc.in), got[:min(len(got), 50)], len(got), len(tc.want))
			}
		})
	}
}
