package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

type testWorkspace struct {
	root            string // project root; empty means CanonPath is identity
	files           map[string]string
	inContext       map[string]bool
	addContextCalls int
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

func (w *testWorkspace) InContext(path string) bool {
	return w.inContext[w.CanonPath(path)]
}

func (w *testWorkspace) AddContext(path string) {
	w.addContextCalls++
	w.inContext[w.CanonPath(path)] = true
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
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)

	if result.Effect != EffectEditProposed {
		t.Fatalf("expected EffectEditProposed, got %d", result.Effect)
	}

	proposal, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("expected EditProposal payload, got %T", result.Payload)
	}

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
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)
	if result.Effect != EffectEditProposed {
		t.Fatalf("expected EffectEditProposed, got %d", result.Effect)
	}

	proposal := result.Payload.(EditProposal)
	if proposal.CanonPath != "/repo/src/main.go" {
		t.Errorf("expected canon path /repo/src/main.go, got %q", proposal.CanonPath)
	}

	// Cache should be populated under the canonical key.
	if _, ok := cache.Get("/repo/src/main.go"); !ok {
		t.Error("expected cache hit for canonical path /repo/src/main.go")
	}

}

func TestEditFileTool_ValidationFailsNoMatch(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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
	if result.Effect != EffectNone {
		t.Errorf("expected EffectNone on validation failure, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "Error:") {
		t.Errorf("expected error message, got %q", result.Content)
	}
}

func TestEditFileTool_ValidationFailsAmbiguous(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "foo\nfoo\nbar"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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
	if result.Effect != EffectNone {
		t.Errorf("expected EffectNone on ambiguous match, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "matches 2 locations") {
		t.Errorf("expected ambiguity error, got %q", result.Content)
	}
}

func TestEditFileTool_EmptySearchOnNonEmptyFile(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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
	if !strings.Contains(result.Content, "search field cannot be empty") {
		t.Errorf("expected empty search error, got %q", result.Content)
	}
}

func TestEditFileTool_ProposalContainsCallID(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)
	proposal := result.Payload.(EditProposal)
	if proposal.Edit.ID != "call-42" {
		t.Errorf("expected edit ID 'call-42', got %q", proposal.Edit.ID)
	}
}

func TestEditFileTool_ProposalPendingEditFields(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"main.go": "hello world"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)
	proposal := result.Payload.(EditProposal)

	want := event.PendingEdit{
		ID:      "1",
		Path:    "main.go",
		Search:  "hello",
		Replace: "goodbye",
		Reason:  "farewell",
	}
	if proposal.Edit != want {
		t.Errorf("PendingEdit mismatch:\n got  %+v\n want %+v", proposal.Edit, want)
	}
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
		files:     map[string]string{"src/player.lua": content},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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
	if result.Effect != EffectNone {
		t.Fatalf("expected EffectNone on duplication reject, got %d (content: %s)", result.Effect, result.Content)
	}
	if !strings.Contains(result.Content, "duplicates content") {
		t.Errorf("expected duplication error, got %q", result.Content)
	}
}

func TestEditFileTool_FuzzyWhitespaceCorrection(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	ws := &testWorkspace{
		files:     map[string]string{"main.go": content},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)
	if result.Effect != EffectEditProposed {
		t.Fatalf("expected EffectEditProposed, got %d (content: %s)", result.Effect, result.Content)
	}
	proposal := result.Payload.(EditProposal)
	// The search should have been corrected to the actual file text.
	if proposal.Edit.Search != "func main() {\n\tfmt.Println(\"hello\")\n}" {
		t.Errorf("search not corrected: %q", proposal.Edit.Search)
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

// TestEditFileTool_UsesCacheConsistentWithBufferAfterContinue
// documents the architectural invariant edit_file relies on: the
// cache reflects buffer content between agent turns, fed by the
// continueCh path that snapshots Buf.Content() on the TUI goroutine
// after each approved edit. The tool reads cache (or falls back to
// disk via ReadFile) — never the buffer directly, because that
// would race against TUI-owned mutations. This test guards against
// a regression that re-introduces direct buffer access from the
// agent goroutine; the test passes when resolveContent only
// consults cache+disk via the FileReader surface.
//
// The cache is primed with content that diverges from disk so a
// regression bypassing cache would (a) read divergent disk bytes
// and (b) fail to match the cache-only Search marker, dropping the
// effect off EffectEditProposed. A ReadFile call counter proves
// disk was not consulted on the cache-hit path.
func TestEditFileTool_UsesCacheConsistentWithBufferAfterContinue(t *testing.T) {
	t.Parallel()

	const diskContent = "package main\n\nfunc main() {}\n"
	const cacheContent = "package main\n\nfunc main() { println(\"cached\") }\n"

	ws := &readCountingWorkspace{
		testWorkspace: &testWorkspace{
			files:     map[string]string{"main.go": diskContent},
			inContext: map[string]bool{},
		},
	}
	cache := NewFileCache()
	cache.Set("main.go", cacheContent) // root="" → CanonPath is identity
	tool := NewEditFileTool(ws, cache)

	args := mustMarshal(t, editArgs{
		Path:    "main.go",
		Search:  `println("cached")`,
		Replace: `println("rewritten")`,
	})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	})

	if result.Effect != EffectEditProposed {
		t.Fatalf("Effect = %d, content = %q; want EffectEditProposed — search "+
			"only matches CACHED content, so a non-proposal result means cache priority broke",
			result.Effect, result.Content)
	}
	prop, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("Payload type = %T, want EditProposal", result.Payload)
	}
	if !strings.Contains(prop.ExpectedContent, `println("rewritten")`) {
		t.Errorf("ExpectedContent did not derive from cached content: %q", prop.ExpectedContent)
	}
	if ws.readCalls != 0 {
		t.Errorf("ReadFile called %d times; want 0 (primed cache must short-circuit disk)",
			ws.readCalls)
	}
}
