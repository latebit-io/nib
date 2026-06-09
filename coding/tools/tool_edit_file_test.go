package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// fakeApprover captures the [EditProposal] passed through the
// approval flow so tests can assert on its fields without standing up
// an Agent. Body and isErr control what Execute sees as the tool
// result; the default zero value is treated as "approved with empty
// body" which is fine for tests that only care about the proposal
// shape.
type fakeApprover struct {
	received *EditProposal
	body     string
	isErr    bool
}

func (f *fakeApprover) Propose(_ context.Context, p EditProposal) (string, bool) {
	f.received = &p
	return f.body, f.isErr
}

type testWorkspace struct {
	root  string // project root; empty means CanonPath is identity
	files map[string]string
}

func (w *testWorkspace) ReadFile(path string) (string, error) {
	if c, ok := w.files[path]; ok {
		return c, nil
	}
	return "", nil
}

func (w *testWorkspace) ListFiles() ([]string, error) { return nil, nil }
func (w *testWorkspace) WriteFile(_, _ string) error  { return nil }
func (w *testWorkspace) CanonPath(p string) string {
	if w.root != "" && !filepath.IsAbs(p) {
		return filepath.Clean(filepath.Join(w.root, p))
	}
	return p
}

func (w *testWorkspace) ProjectRoot() string { return w.root }

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestEditFileTool_ReturnsEditProposal(t *testing.T) {
	ws := &testWorkspace{
		files: map[string]string{"src/main.go": "package main"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	tool.Execute(context.Background(), call)

	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}
	proposal := *app.received

	if proposal.Edit.Path != "src/main.go" {
		t.Errorf("expected path src/main.go, got %q", proposal.Edit.Path)
	}
	if proposal.Edit.Search != "package main" {
		t.Errorf("expected search 'package main', got %q", proposal.Edit.Search)
	}
	if proposal.Edit.Replace != "package foo" {
		t.Errorf("expected replace 'package foo', got %q", proposal.Edit.Replace)
	}
	if proposal.ExpectedContent != "package foo" {
		t.Errorf("expected content 'package foo', got %q", proposal.ExpectedContent)
	}
}

func TestEditFileTool_RootedCanonPath(t *testing.T) {
	// Exercises the root-aware CanonPath branch: relative paths are resolved
	// against the workspace root, and the canonical key is used for cache
	// lookups and context tracking.
	ws := &testWorkspace{
		root: "/repo",
		// File keyed by original relative path — the closure in resolveContent
		// passes the original path to ReadFile, not the canonical key.
		files: map[string]string{"src/main.go": "package main"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	tool.Execute(context.Background(), call)
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}

	if app.received.CanonPath != "/repo/src/main.go" {
		t.Errorf("expected canon path /repo/src/main.go, got %q", app.received.CanonPath)
	}

	// Cache should be populated under the canonical key.
	if _, ok := cache.Get("/repo/src/main.go"); !ok {
		t.Error("expected cache hit for canonical path /repo/src/main.go")
	}

}

func TestEditFileTool_ValidationFailsNoMatch(t *testing.T) {
	ws := &testWorkspace{
		files: map[string]string{"src/main.go": "package main"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "nonexistent text",
		Replace: "replacement",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if app.received != nil {
		t.Errorf("approver must not see a proposal on validation failure, received %+v", app.received)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true on validation failure")
	}
	if !strings.Contains(result.Content, "Error:") {
		t.Errorf("expected error message, got %q", result.Content)
	}
}

func TestEditFileTool_ValidationFailsAmbiguous(t *testing.T) {
	ws := &testWorkspace{
		files: map[string]string{"src/main.go": "foo\nfoo\nbar"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "foo",
		Replace: "baz",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if app.received != nil {
		t.Errorf("approver must not see a proposal on ambiguity, received %+v", app.received)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true on ambiguous match")
	}
	if !strings.Contains(result.Content, "matches 2 locations") {
		t.Errorf("expected ambiguity error, got %q", result.Content)
	}
}

func TestEditFileTool_EmptySearchOnNonEmptyFile(t *testing.T) {
	ws := &testWorkspace{
		files: map[string]string{"src/main.go": "package main"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "",
		Replace: "new content",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if app.received != nil {
		t.Errorf("approver must not see a proposal on empty-search guard, received %+v", app.received)
	}
	if !strings.Contains(result.Content, "search field cannot be empty") {
		t.Errorf("expected empty search error, got %q", result.Content)
	}
}

func TestEditFileTool_ProposalContainsCallID(t *testing.T) {
	ws := &testWorkspace{
		files: map[string]string{"src/main.go": "package main"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "call-42",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	tool.Execute(context.Background(), call)
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}
	if app.received.Edit.ID != "call-42" {
		t.Errorf("expected edit ID 'call-42', got %q", app.received.Edit.ID)
	}
}

func TestEditFileTool_ProposalPendingEditFields(t *testing.T) {
	ws := &testWorkspace{
		files: map[string]string{"main.go": "hello world"},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "main.go",
		Search:  "hello",
		Replace: "goodbye",
		Reason:  "farewell",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	tool.Execute(context.Background(), call)
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}

	want := event.PendingEdit{
		ID:      "1",
		Path:    "main.go",
		Search:  "hello",
		Replace: "goodbye",
		Reason:  "farewell",
	}
	if app.received.Edit != want {
		t.Errorf("PendingEdit mismatch:\n got  %+v\n want %+v", app.received.Edit, want)
	}
}

// TestEditOverlapRatio locks in the over-rewrite signal: a high ratio of
// shared lines between search and replace flags edits that rewrote more
// than they needed to change. Cases cover the extremes (clean rewrite =
// 0.0, identical = 1.0), partial overlap, and the noise-suppression
// rules (empty lines and pure-whitespace lines are excluded).
func TestEditOverlapRatio(t *testing.T) {
	tests := []struct {
		name      string
		search    string
		replace   string
		wantRatio float64
		wantLines int
	}{
		{
			name:      "identical text — full overlap",
			search:    "a\nb\nc",
			replace:   "a\nb\nc",
			wantRatio: 1.0,
			wantLines: 3,
		},
		{
			name:      "totally different — zero overlap",
			search:    "a\nb\nc",
			replace:   "x\ny\nz",
			wantRatio: 0.0,
			wantLines: 3,
		},
		{
			name:      "two of three lines unchanged",
			search:    "a\nb\nc",
			replace:   "a\nNEW\nc",
			wantRatio: 2.0 / 3.0,
			wantLines: 3,
		},
		{
			name:      "duplicate lines counted via multiset",
			search:    "a\na\nb",
			replace:   "a\nb\nb",
			wantRatio: 2.0 / 3.0, // one a + one b match; duplicate replace-b not reused
			wantLines: 3,
		},
		{
			name:      "blank lines excluded from both sides",
			search:    "a\n\n\nb",
			replace:   "a\nb",
			wantRatio: 1.0,
			wantLines: 2,
		},
		{
			name:      "pure-whitespace lines excluded",
			search:    "a\n   \nb",
			replace:   "a\nb",
			wantRatio: 1.0,
			wantLines: 2,
		},
		{
			name:      "empty search returns zero-zero",
			search:    "",
			replace:   "anything",
			wantRatio: 0.0,
			wantLines: 0,
		},
		{
			name:      "rewrote-whole-function with one-field added",
			search:    "func F(name string) {\n\treturn name\n}",
			replace:   "func F(name string) {\n\tlog.Print(\"hi\")\n\treturn name\n}",
			wantRatio: 1.0, // every non-empty search line appears unchanged in replace
			wantLines: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRatio, gotLines := editOverlapRatio(tt.search, tt.replace)
			if gotLines != tt.wantLines {
				t.Errorf("lines = %d, want %d", gotLines, tt.wantLines)
			}
			// Allow a small floating-point tolerance for division results.
			delta := gotRatio - tt.wantRatio
			if delta < -1e-9 || delta > 1e-9 {
				t.Errorf("ratio = %v, want %v", gotRatio, tt.wantRatio)
			}
		})
	}
}

// stripLineNumberCase describes one stripLineNumberPrefixes scenario.
// Shared between the basic-rules and contiguity-validation suites so
// both groups assert with identical semantics; splitting the table is
// purely a function-length concern.
type stripLineNumberCase struct {
	name string
	in   string
	want string
}

// runStripLineNumberCases is the shared runner. Helper-marked so failures
// surface against the calling test, not this row processor.
func runStripLineNumberCases(t *testing.T, cases []stripLineNumberCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := stripLineNumberPrefixes(tt.in)
			if got != tt.want {
				t.Errorf("stripLineNumberPrefixes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestStripLineNumberPrefixes covers the deterministic strip applied to
// edit_file's search field before matching. The strip exists to fix a
// recurring failure mode where the model copies prompt-formatted file
// content (with `   N | ` or `   N\t` prefixes) into the search field
// verbatim, then sees a no-match because the prefix is never in the real
// file. This group exercises the basic apply / skip rules; the stricter
// contiguity validation has its own group.
func TestStripLineNumberPrefixes(t *testing.T) {
	runStripLineNumberCases(t, []stripLineNumberCase{
		// --- Strip applied: every non-blank line carries a prefix ---
		{
			name: "buildMessages pipe format",
			in:   "   1 | package main\n   2 | \n   3 | func main() {}\n",
			want: "package main\n\nfunc main() {}\n",
		},
		{
			name: "read_file tab format",
			in:   "   1\tpackage main\n   2\tfunc main() {}\n",
			want: "package main\nfunc main() {}\n",
		},
		{name: "no padding pipe", in: "1 | foo\n2 | bar", want: "foo\nbar"},
		{name: "wide line numbers", in: " 999 | foo\n1000 | bar", want: "foo\nbar"},

		// --- Strip skipped: at least one non-blank line lacks a prefix ---
		{name: "no prefix at all", in: "package main\nfunc main() {}", want: "package main\nfunc main() {}"},
		{
			name: "mixed (one line without prefix)",
			in:   "   1 | package main\nbare line\n   3 | foo",
			want: "   1 | package main\nbare line\n   3 | foo",
		},
		{
			name: "literal markdown row that resembles prefix",
			in:   "1.0 | description",
			want: "1.0 | description", // not stripped — leading "1.0" doesn't match `\d+ | `
		},

		// --- Edge cases ---
		{name: "empty string", in: "", want: ""},
		{name: "only blank lines", in: "\n\n", want: "\n\n"},
		{name: "single prefixed line", in: "  42 | the answer", want: "the answer"},
	})
}

// TestStripLineNumberPrefixes_Contiguity covers the stricter validation
// added 2026-04-27: a prompt excerpt always uses one separator and shows
// consecutive line numbers. Tables or hand-assembled blocks that happen
// to match the regex per-line MUST NOT be silently rewritten.
func TestStripLineNumberPrefixes_Contiguity(t *testing.T) {
	runStripLineNumberCases(t, []stripLineNumberCase{
		// --- Contiguity validation: condition 3 ---
		// A real prompt excerpt always shows consecutive line numbers.
		// Tables that happen to look like prefixes (e.g. a markdown row
		// "1 | A" / "3 | C" / "5 | E") MUST NOT be silently rewritten.
		{name: "non-contiguous numbers — leave alone", in: "1 | A\n3 | C\n5 | E", want: "1 | A\n3 | C\n5 | E"},
		{name: "off-by-one gap — leave alone", in: "10 | foo\n11 | bar\n13 | baz", want: "10 | foo\n11 | bar\n13 | baz"},
		{name: "decreasing numbers — leave alone", in: "3 | a\n2 | b\n1 | c", want: "3 | a\n2 | b\n1 | c"},
		{name: "duplicate number — leave alone", in: "1 | a\n1 | b", want: "1 | a\n1 | b"},

		// --- Separator-consistency validation: condition 2 ---
		// A genuine excerpt comes from one prompt surface, so the
		// separator (` | ` vs `\t`) is uniform. Mixed separators imply
		// hand-assembled content; do not strip.
		{name: "mixed separators — leave alone", in: "  1 | first\n  2\tsecond", want: "  1 | first\n  2\tsecond"},

		// --- Sanity: contiguity does NOT require starting at 1 ---
		// Read_file with offset=42 produces "  42\tfoo" / "  43\tbar".
		// The strip should still apply.
		{name: "contiguous starting from non-1 with tab", in: "  42\tfoo\n  43\tbar\n  44\tbaz", want: "foo\nbar\nbaz"},
		{name: "contiguous starting from non-1 with pipe", in: " 100 | foo\n 101 | bar", want: "foo\nbar"},
	})
}

func TestFuzzyWhitespaceMatch(t *testing.T) {
	t.Run("tabs vs spaces", func(t *testing.T) {
		content := "func main() {\n\tfmt.Println(\"hello\")\n}"
		search := "func main() {\n    fmt.Println(\"hello\")\n}"
		got := fuzzyWhitespaceMatch(search, content)
		if got != "func main() {\n\tfmt.Println(\"hello\")\n}" {
			t.Errorf("expected file text, got %q", got)
		}
	})

	t.Run("wrong indentation depth", func(t *testing.T) {
		content := "\t\tif err != nil {\n\t\t\treturn err\n\t\t}"
		search := "\tif err != nil {\n\t\treturn err\n\t}"
		got := fuzzyWhitespaceMatch(search, content)
		if got != content {
			t.Errorf("expected file text, got %q", got)
		}
	})

	t.Run("exact match returns empty", func(t *testing.T) {
		content := "line one\nline two"
		search := "line one\nline two"
		got := fuzzyWhitespaceMatch(search, content)
		if got != "" {
			t.Errorf("exact match should return empty, got %q", got)
		}
	})

	t.Run("multiple matches returns empty", func(t *testing.T) {
		content := "\tfoo\n\tbar\n\tfoo\n\tbar"
		search := "  foo\n  bar"
		got := fuzzyWhitespaceMatch(search, content)
		if got != "" {
			t.Errorf("multiple matches should return empty, got %q", got)
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		content := "func main() {}"
		search := "func other() {}"
		got := fuzzyWhitespaceMatch(search, content)
		if got != "" {
			t.Errorf("no match should return empty, got %q", got)
		}
	})
}

// corruptionCase describes one scenario for detectLikelyCorruption: the
// inputs and whether the result is expected to be flagged. wantFlag is true
// when detectLikelyCorruption should return a non-empty error; matchSub is
// a substring the returned error must contain when flagged.
type corruptionCase struct {
	name     string
	search   string
	replace  string
	content  string
	wantFlag bool
	matchSub string
}

// runCorruptionCases is the shared table-driven body for every
// TestDetectLikelyCorruption_* group.
func runCorruptionCases(t *testing.T, cases []corruptionCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectLikelyCorruption(tc.search, tc.replace, tc.content)
			if tc.wantFlag {
				if got == "" {
					t.Fatalf("expected corruption to be flagged, got clean result")
				}
				if !strings.Contains(got, tc.matchSub) {
					t.Errorf("error message %q does not contain %q", got, tc.matchSub)
				}
				return
			}
			if got != "" {
				t.Errorf("expected no flag, got %q", got)
			}
		})
	}
}

func TestDetectLikelyCorruption_AllowsSafeEdits(t *testing.T) {
	runCorruptionCases(t, []corruptionCase{
		{
			name:    "safe small inline edit",
			search:  "foo",
			replace: "bar",
			content: "foo baz",
		},
		{
			name:    "plain deletion is always allowed",
			search:  "dead line\n",
			replace: "",
			content: "keep\ndead line\ntail\n",
		},
	})
}

func TestDetectLikelyCorruption_MidLineTruncation(t *testing.T) {
	// longMidLineReplace mimics an LLM output that hit max_tokens mid-string:
	// search ends with a newline (as multi-line edits normally do) but the
	// replacement is long and stops mid-line.
	longMidLineReplace := strings.Repeat("x ", midLineTruncationFloor/2+10) + "unterminated"

	runCorruptionCases(t, []corruptionCase{
		{
			name:     "mid-line truncation signature",
			search:   "old block line\n",
			replace:  longMidLineReplace,
			content:  "prefix\nold block line\nsuffix\n",
			wantFlag: true,
			matchSub: "truncated mid-line",
		},
		{
			name:    "short mid-line replace is not flagged",
			search:  "old\n",
			replace: "new",
			content: "prefix\nold\nsuffix\n",
		},
		// Exact boundary on midLineTruncationFloor: >= is inclusive.
		{
			name:     "replace exactly at mid-line truncation floor flags",
			search:   "anchor\n",
			replace:  strings.Repeat("a", midLineTruncationFloor),
			content:  "prefix\nanchor\nsuffix\n",
			wantFlag: true,
			matchSub: "truncated mid-line",
		},
		{
			name:    "replace one byte below mid-line truncation floor does not flag",
			search:  "anchor\n",
			replace: strings.Repeat("a", midLineTruncationFloor-1),
			content: "prefix\nanchor\nsuffix\n",
		},
	})
}

func TestDetectLikelyCorruption_Duplication(t *testing.T) {
	runCorruptionCases(t, []corruptionCase{
		{
			// Regression: matches the corrupted src/player.lua pattern where
			// the agent matched a short prefix and tried to "restore" the
			// entire tail, duplicating it.
			name:   "suffix duplication (agent rewrites existing tail)",
			search: "return launchRes",
			replace: "return launchResult\n\tend\n\tspendSeekerSwarmEnergy(activeCardOrder.ship)\n" +
				"\treturn okResult()\nend\n\nreturn player",
			content: "header\n" +
				"return launchResult\n\tend\n\tspendSeekerSwarmEnergy(activeCardOrder.ship)\n" +
				"\treturn okResult()\nend\n\nreturn player\n",
			wantFlag: true,
			matchSub: "duplicates content",
		},
		{
			name:    "non-duplicating tail replace is allowed",
			search:  "return old\n",
			replace: "return new with a decently long replacement to clear short-length guards\n",
			content: "header\nreturn old\nunrelated tail content here\n",
		},
		{
			name:    "replace exactly at duplication probe boundary flags",
			search:  "X",
			replace: strings.Repeat("b", duplicationProbeBytes),
			content: "X" + strings.Repeat("c", 10) +
				strings.Repeat("b", duplicationProbeBytes) +
				strings.Repeat("c", 10),
			wantFlag: true,
			matchSub: "duplicates content",
		},
		{
			name:    "replace one byte below duplication probe boundary does not flag",
			search:  "X",
			replace: strings.Repeat("b", duplicationProbeBytes-1),
			content: "X" + strings.Repeat("c", 10) +
				strings.Repeat("b", duplicationProbeBytes-1) +
				strings.Repeat("c", 10),
		},
		{
			// Regression: the probe must only scan near the match boundary.
			// A coincidental match far in the file's tail (e.g. a common
			// error-handling idiom reused in a later function) is not
			// boundary-crossing duplication and must not be flagged.
			name:    "far-tail coincidental match does not flag",
			search:  "X",
			replace: "return fmt.Errorf(\"something specific: %w\", err)\n\treturn nil\n\t}\n\t}",
			content: "X" +
				strings.Repeat("unrelated code line\n", 200) +
				"return fmt.Errorf(\"something specific: %w\", err)\n\treturn nil\n\t}\n\t}",
		},
	})
}

func TestEditFileTool_RejectsDuplicationEdit(t *testing.T) {
	// End-to-end check: the defensive guard surfaces as a tool error so the
	// LLM gets a chance to retry with a narrower edit instead of silently
	// corrupting the file.
	tail := "return launchResult\n\tend\n\tspendSeekerSwarmEnergy(activeCardOrder.ship)\n" +
		"\treturn okResult()\nend\n\nreturn player\n"
	content := "header\n" + tail
	ws := &testWorkspace{
		files: map[string]string{"src/player.lua": content},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "src/player.lua",
		Search:  "return launchRes",
		Replace: "return launchResult\n\tend\n\tspendSeekerSwarmEnergy(activeCardOrder.ship)\n\treturn okResult()\nend\n\nreturn player",
		Reason:  "restore tail",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if app.received != nil {
		t.Fatalf("approver must not see a proposal on duplication reject, received %+v", app.received)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true on duplication reject")
	}
	if !strings.Contains(result.Content, "duplicates content") {
		t.Errorf("expected duplication error, got %q", result.Content)
	}
}

func TestEditFileTool_FuzzyWhitespaceCorrection(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	ws := &testWorkspace{
		files: map[string]string{"main.go": content},
	}
	cache := NewFileCache()
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	// LLM sends spaces instead of tabs — fuzzy match should correct it.
	args := mustMarshal(t, editArgs{
		Path:    "main.go",
		Search:  "func main() {\n    fmt.Println(\"hello\")\n}",
		Replace: "func main() {\n\tfmt.Println(\"goodbye\")\n}",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	tool.Execute(context.Background(), call)
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}
	// The search should have been corrected to the actual file text.
	if app.received.Edit.Search != "func main() {\n\tfmt.Println(\"hello\")\n}" {
		t.Errorf("search not corrected: %q", app.received.Edit.Search)
	}
}

// readCountingWorkspace wraps testWorkspace to record ReadFile calls,
// proving that a primed cache short-circuits disk access in
// resolveContent. Used by the cache-priority regression test.
type readCountingWorkspace struct {
	*testWorkspace
	readCalls int
}

func (w *readCountingWorkspace) ReadFile(path string) (string, error) {
	w.readCalls++
	return w.testWorkspace.ReadFile(path)
}

// TestEditFileTool_UsesCachePostApproval documents the architectural
// invariant edit_file relies on: the cache reflects post-approval
// content between agent turns, seeded by the orchestrator after each
// approved edit. The tool reads cache (or falls back to disk via
// ReadFile) — never the buffer directly, because that would race
// against TUI-owned mutations. This test guards against a regression
// that re-introduces direct buffer access from the agent goroutine;
// the test passes when resolveContent only consults cache+disk via
// the FileReader surface.
//
// The cache is primed with content that diverges from disk so a
// regression bypassing cache would (a) read divergent disk bytes
// and (b) fail to match the cache-only Search marker, dropping the
// effect off EffectEditProposed. A ReadFile call counter proves
// disk was not consulted on the cache-hit path.
func TestEditFileTool_UsesCachePostApproval(t *testing.T) {
	t.Parallel()

	const diskContent = "package main\n\nfunc main() {}\n"
	const cacheContent = "package main\n\nfunc main() { println(\"cached\") }\n"

	ws := &readCountingWorkspace{
		testWorkspace: &testWorkspace{
			files: map[string]string{"main.go": diskContent},
		},
	}
	cache := NewFileCache()
	cache.Set("main.go", cacheContent) // root="" → CanonPath is identity
	app := &fakeApprover{}
	tool := NewEditFileTool(ws, cache, app)

	args := mustMarshal(t, editArgs{
		Path:    "main.go",
		Search:  `println("cached")`,
		Replace: `println("rewritten")`,
	})
	tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	})

	if app.received == nil {
		t.Fatalf("approver did not receive a proposal — search only matches "+
			"CACHED content, so a missing proposal means cache priority broke. "+
			"ReadFile calls=%d", ws.readCalls)
	}
	if !strings.Contains(app.received.ExpectedContent, `println("rewritten")`) {
		t.Errorf("ExpectedContent did not derive from cached content: %q", app.received.ExpectedContent)
	}
	if ws.readCalls != 0 {
		t.Errorf("ReadFile called %d times; want 0 (primed cache must short-circuit disk)",
			ws.readCalls)
	}
}
