package architecture

import (
	"context"
	"strings"
	"sync"
	"testing"

	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"

	"github.com/latebit-io/junto/engine/styleconfig"
	"github.com/latebit-io/junto/engine/validate"
)

// stubProvider is the test double for [Provider]. The Architecture is
// guarded by a mutex so race tests can mutate it concurrently with
// Validate calls.
type stubProvider struct {
	mu   sync.Mutex
	arch styleconfig.Architecture
}

func (p *stubProvider) Architecture() styleconfig.Architecture {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.arch
}

func (p *stubProvider) set(a styleconfig.Architecture) {
	p.mu.Lock()
	p.arch = a
	p.mu.Unlock()
}

// langFor wires Lua and Go grammars to their conventional extensions.
// Anything else is treated as unsupported.
var (
	luaLang = sitter.NewLanguage(tree_sitter_lua.Language())
	goLang  = sitter.NewLanguage(tree_sitter_go.Language())
)

func langFor(path string) *sitter.Language {
	switch {
	case strings.HasSuffix(path, ".lua"):
		return luaLang
	case strings.HasSuffix(path, ".go"):
		return goLang
	default:
		return nil
	}
}

// TestApplicableGates verifies the validator skips work in three cases:
// nil provider, disabled architecture, and unsupported language. Files
// the grammar registry does not recognise (config, docs) must never
// trigger architectural caps.
func TestApplicableGates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mkValid func() *Validator
		path    string
		want    bool
	}{
		{
			name: "nil provider",
			mkValid: func() *Validator {
				return New(nil, langFor)
			},
			path: "main.lua",
			want: false,
		},
		{
			name: "architecture disabled",
			mkValid: func() *Validator {
				return New(&stubProvider{}, langFor)
			},
			path: "main.lua",
			want: false,
		},
		{
			name: "action off overrides caps",
			mkValid: func() *Validator {
				return New(&stubProvider{arch: styleconfig.Architecture{
					MaxFileLines: 100, Action: "off",
				}}, langFor)
			},
			path: "main.lua",
			want: false,
		},
		{
			name: "unsupported language",
			mkValid: func() *Validator {
				return New(&stubProvider{arch: styleconfig.Architecture{
					MaxFileLines: 100,
				}}, langFor)
			},
			path: "config.json",
			want: false,
		},
		{
			name: "nil langFor",
			mkValid: func() *Validator {
				return New(&stubProvider{arch: styleconfig.Architecture{
					MaxFileLines: 100,
				}}, nil)
			},
			path: "main.lua",
			want: false,
		},
		{
			name: "lua file with caps active",
			mkValid: func() *Validator {
				return New(&stubProvider{arch: styleconfig.Architecture{
					MaxFileLines: 100,
				}}, langFor)
			},
			path: "main.lua",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.mkValid()
			got := v.Applicable(validate.Candidate{Path: tc.path})
			if got != tc.want {
				t.Errorf("Applicable(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestFileLineCapRetry verifies a soft (action="warn") cap produces a
// Retry verdict with feedback that includes both line counts so the
// LLM can localise the problem.
func TestFileLineCapRetry(t *testing.T) {
	t.Parallel()

	src := strings.Repeat("local x = 1\n", 200) // 200 lines
	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFileLines: 50, Action: "warn",
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: src,
	})

	if got.Verdict != validate.Retry {
		t.Errorf("Verdict = %v, want Retry", got.Verdict)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(got.Findings))
	}
	if !strings.Contains(got.Findings[0].Message, "200") || !strings.Contains(got.Findings[0].Message, "50") {
		t.Errorf("finding message %q does not name actual or cap line count", got.Findings[0].Message)
	}
	if !strings.Contains(got.Feedback, "main.lua") {
		t.Errorf("feedback missing path; got %q", got.Feedback)
	}
}

// TestFileLineCapBlock verifies action="block" produces Verdict=Block
// (not Retry) so the developer sees the issue without a silent retry.
func TestFileLineCapBlock(t *testing.T) {
	t.Parallel()

	src := strings.Repeat("local x = 1\n", 200)
	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFileLines: 50, Action: "block",
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: src,
	})

	if got.Verdict != validate.Block {
		t.Errorf("Verdict = %v, want Block", got.Verdict)
	}
}

// TestFileLineCapPassUnderLimit verifies Pass when a file is under the
// cap. No findings, no feedback.
func TestFileLineCapPassUnderLimit(t *testing.T) {
	t.Parallel()

	src := strings.Repeat("local x = 1\n", 10)
	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFileLines: 50,
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: src,
	})

	if got.Verdict != validate.Pass {
		t.Errorf("Verdict = %v, want Pass", got.Verdict)
	}
	if len(got.Findings) != 0 {
		t.Errorf("Findings = %d, want 0", len(got.Findings))
	}
}

// TestFunctionLineCap covers the per-function cap. Tree-sitter parses
// the Lua function declaration; the validator flags it because the
// body spans more lines than the cap.
func TestFunctionLineCap(t *testing.T) {
	t.Parallel()

	src := "function huge()\n" + strings.Repeat("    print(1)\n", 30) + "end\n"

	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFunctionLines: 10,
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: src,
	})

	if got.Verdict != validate.Retry {
		t.Errorf("Verdict = %v, want Retry; feedback=%q", got.Verdict, got.Feedback)
	}
	if len(got.Findings) == 0 {
		t.Fatalf("Findings empty; want at least one")
	}
	if !strings.Contains(got.Feedback, "Extract helpers") {
		t.Errorf("feedback missing extract-helpers guidance: %q", got.Feedback)
	}
}

// TestFunctionsPerFileCap verifies the count cap. Three small functions
// in one file, cap of 2, expects Retry.
func TestFunctionsPerFileCap(t *testing.T) {
	t.Parallel()

	src := "" +
		"function a() end\n" +
		"function b() end\n" +
		"function c() end\n"

	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFunctionsPerFile: 2,
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: src,
	})

	if got.Verdict != validate.Retry {
		t.Errorf("Verdict = %v, want Retry", got.Verdict)
	}
	if !strings.Contains(got.Feedback, "3 functions") {
		t.Errorf("feedback missing function count: %q", got.Feedback)
	}
}

// TestGoFunctionDetection sanity-checks that Go function and method
// declarations are picked up by the same validator without per-language
// special cases — proving the kind-substring heuristic is portable.
func TestGoFunctionDetection(t *testing.T) {
	t.Parallel()

	src := "" +
		"package main\n" +
		"\n" +
		"func a() {}\n" +
		"func b() {}\n" +
		"func c() {}\n"

	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFunctionsPerFile: 2,
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.go",
		After: src,
	})

	if got.Verdict != validate.Retry {
		t.Errorf("Verdict = %v, want Retry; got findings=%v", got.Verdict, got.Findings)
	}
}

// TestSnapshotIsolatedFromConcurrentSet verifies the architecture
// snapshot taken at the start of Validate is not affected by a
// concurrent Set. Without snapshotting, this test would be racy under
// `go test -race`.
func TestSnapshotIsolatedFromConcurrentSet(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{arch: styleconfig.Architecture{
		MaxFileLines: 50, Action: "warn",
	}}
	v := New(provider, langFor)

	src := strings.Repeat("local x = 1\n", 200)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			provider.set(styleconfig.Architecture{MaxFileLines: 10})
			provider.set(styleconfig.Architecture{MaxFileLines: 1000})
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			_ = v.Validate(context.Background(), validate.Candidate{
				Path:  "main.lua",
				After: src,
			})
		}
	}()
	wg.Wait()
}

// TestNoFunctionGrammarStillCheckesFileLines verifies that file-line
// caps still fire even when the function-bound parsing portion of the
// validator finds zero functions (e.g. a YAML-like file). The
// applicability gate already requires a registered language; this test
// covers the "language registered but has no functions" case which
// would still benefit from the file-line cap.
func TestFileLineCapAppliesEvenWhenNoFunctions(t *testing.T) {
	t.Parallel()

	// Lua source with no function declarations — pure data.
	src := strings.Repeat("local x = 1\n", 200)

	v := New(&stubProvider{arch: styleconfig.Architecture{
		MaxFileLines:        50,
		MaxFunctionLines:    20,
		MaxFunctionsPerFile: 10,
	}}, langFor)

	got := v.Validate(context.Background(), validate.Candidate{
		Path:  "main.lua",
		After: src,
	})

	if got.Verdict != validate.Retry {
		t.Errorf("Verdict = %v, want Retry", got.Verdict)
	}
	if len(got.Findings) != 1 {
		t.Errorf("Findings = %d, want 1 (file-line only)", len(got.Findings))
	}
}

// TestCountLinesNoTrailingNewline verifies countLines treats a file
// without a trailing newline as having all its visible lines counted —
// matching wc -l's lines-of-content semantics.
func TestCountLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src  string
		want int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a", 1},
		{"a\nb", 2},
		{"\n", 1},
	}
	for _, tc := range cases {
		got := countLines(tc.src)
		if got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.src, got, tc.want)
		}
	}
}

// TestNameIsStable locks in the stage identifier so capture-payload
// consumers can match on it without risk of silent rename.
func TestNameIsStable(t *testing.T) {
	t.Parallel()

	if got := (Validator{}).Name(); got != StageName {
		t.Errorf("Name() = %q, want %q", got, StageName)
	}
}
