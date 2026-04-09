package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

type styleLintTestCase struct {
	name      string
	cmds      []string
	relPath   string
	wantEmpty bool
	contains  []string
}

var styleLintTests = []styleLintTestCase{
	{name: "no commands configured", cmds: nil, wantEmpty: true},
	{name: "command with output", cmds: []string{`echo "violation found"`}, contains: []string{"violation found"}},
	{name: "command with no output", cmds: []string{"true"}, wantEmpty: true},
	{name: "non-zero exit with output", cmds: []string{`echo "error: bad style" && exit 1`}, contains: []string{"error: bad style"}},
	{name: "file placeholder substituted", cmds: []string{`echo "checking {file}"`}, relPath: "src/main.go", contains: []string{"checking src/main.go"}},
	{name: "dir placeholder filters to edited file", cmds: []string{`echo "internal/ui/app.go:10:1: missing doc" && echo "internal/ui/other.go:5:1: noise" && echo "linting ./{dir}"`}, relPath: "internal/ui/app.go", contains: []string{"app.go:10:1"}},
	{name: "multiple commands concatenated", cmds: []string{`echo "lint1: issue"`, `echo "lint2: issue"`}, contains: []string{"lint1: issue", "lint2: issue"}},
	{name: "multiple commands one silent", cmds: []string{"true", `echo "only this"`}, contains: []string{"only this"}},
	{name: "command not found", cmds: []string{"nonexistent_lint_tool_xyz123"}, contains: []string{"not found"}},
	{name: "shell metacharacters in path rejected", cmds: []string{`echo "checking {file}"`}, relPath: "src/'; rm -rf / #.go", contains: []string{"skipped", "shell metacharacters"}},
}

func TestRunStyleLint(t *testing.T) {
	for _, tc := range styleLintTests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{
				workspace:    promptTestWorkspace{},
				styleLintCmd: tc.cmds,
			}
			relPath := tc.relPath
			if relPath == "" {
				relPath = "test.go"
			}
			result := a.runStyleLint(context.Background(), relPath)
			assertLintResult(t, result, tc)
		})
	}
}

func assertLintResult(t *testing.T, result string, tc styleLintTestCase) {
	t.Helper()
	if tc.wantEmpty {
		if result != "" {
			t.Errorf("expected empty result, got %q", result)
		}
		return
	}
	if result == "" {
		t.Fatal("expected non-empty result, got empty")
	}
	for _, s := range tc.contains {
		if !strings.Contains(result, s) {
			t.Errorf("result should contain %q, got:\n%s", s, result)
		}
	}
}

func TestRunStyleLint_timeout(t *testing.T) {
	a := &Agent{
		workspace:    promptTestWorkspace{},
		styleLintCmd: []string{`echo "started" && sleep 60`},
		lintTimeout:  200 * time.Millisecond,
	}

	result := a.runStyleLint(context.Background(), "test.go")
	if !strings.Contains(result, "timed out") {
		t.Errorf("expected timeout indicator, got %q", result)
	}
	if !strings.Contains(result, "started") {
		t.Errorf("expected partial output before timeout, got %q", result)
	}
}

func TestRunLintCommand(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		wantEmpty bool
		contains  string
	}{
		{
			name:     "echo output",
			cmd:      `echo "hello lint"`,
			contains: "hello lint",
		},
		{
			name:      "no output",
			cmd:       "true",
			wantEmpty: true,
		},
		{
			name:     "stderr captured",
			cmd:      `echo "stderr msg" >&2`,
			contains: "stderr msg",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := runLintCommand(context.Background(), "", tc.cmd, defaultLintTimeout)

			if tc.wantEmpty {
				if result != "" {
					t.Errorf("expected empty, got %q", result)
				}
				return
			}
			if !strings.Contains(result, tc.contains) {
				t.Errorf("result should contain %q, got %q", tc.contains, result)
			}
		})
	}
}

func TestFilterLintOutput(t *testing.T) {
	output := `internal/ui/tui.go:45:2: exported function Foo should have comment (golint)
internal/ui/todo.go:12:5: error return value not checked (errcheck)
internal/ui/todo.go:30:1: function too long (funlen)
internal/ui/model.go:8:1: missing doc comment (golint)`

	got := filterLintOutput(output, "internal/ui/todo.go", "todo.go")
	if !strings.Contains(got, "todo.go:12:5") {
		t.Errorf("should contain todo.go:12 violation, got:\n%s", got)
	}
	if !strings.Contains(got, "todo.go:30:1") {
		t.Errorf("should contain todo.go:30 violation, got:\n%s", got)
	}
	if strings.Contains(got, "tui.go") {
		t.Errorf("should not contain tui.go violations, got:\n%s", got)
	}
	if strings.Contains(got, "model.go") {
		t.Errorf("should not contain model.go violations, got:\n%s", got)
	}
}

func TestFilterLintOutput_noMatches(t *testing.T) {
	output := `other.go:1:1: some issue`
	got := filterLintOutput(output, "main.go", "main.go")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSafeForShell(t *testing.T) {
	safe := []string{"src/main.go", "internal/ui/app.go", "file-name_v2.txt", "a/b/c.rs"}
	for _, s := range safe {
		if !safeForShell(s) {
			t.Errorf("safeForShell(%q) = false, want true", s)
		}
	}
	unsafe := []string{
		"",
		"file name.go",
		"src/';echo pwned",
		"$(whoami).go",
		"file`id`.go",
		"a|b.go",
		"a&b.go",
		"a;b.go",
		"file\nname.go",
	}
	for _, s := range unsafe {
		if safeForShell(s) {
			t.Errorf("safeForShell(%q) = true, want false", s)
		}
	}
}

func TestRunStyleLint_concurrencySafe(t *testing.T) {
	// Verify that SetStyleLintCmd and runStyleLint don't race.
	a := &Agent{
		workspace:    promptTestWorkspace{},
		styleLintCmd: []string{`echo "initial"`},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			a.SetStyle(nil, []string{`echo "updated"`})
		}
	}()

	for range 10 {
		a.runStyleLint(context.Background(), "test.go")
	}
	<-done
}
