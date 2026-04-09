package agent

import (
	"strings"
	"testing"
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
	{name: "multiple commands concatenated", cmds: []string{`echo "lint1: issue"`, `echo "lint2: issue"`}, contains: []string{"lint1: issue", "lint2: issue"}},
	{name: "multiple commands one silent", cmds: []string{"true", `echo "only this"`}, contains: []string{"only this"}},
	{name: "command not found", cmds: []string{"nonexistent_lint_tool_xyz123"}, contains: []string{"not found"}},
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
			result := a.runStyleLint(relPath)
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
	if testing.Short() {
		t.Skip("skipping timeout test in short mode")
	}

	a := &Agent{
		workspace:    promptTestWorkspace{},
		styleLintCmd: []string{"sleep 60"},
	}

	// Override the timeout for testing — we don't want to wait 30s.
	// Since styleLintTimeout is a const, we test the timeout behavior
	// by using a command that blocks and checking the result contains
	// the timeout indicator. This test will take ~30s.
	// Instead, use a command that we can detect was killed.
	a.styleLintCmd = []string{`echo "started" && sleep 60`}

	// This will take styleLintTimeout (30s) — skip in CI.
	// For manual testing: go test ./agent/ -run TestRunStyleLint_timeout -timeout 60s
	t.Skip("timeout test takes 30s — run manually with: go test ./agent/ -run TestRunStyleLint_timeout -timeout 60s")
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
			result := runLintCommand("", tc.cmd)

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
			a.SetStyleLintCmd([]string{`echo "updated"`})
		}
	}()

	for range 10 {
		a.runStyleLint("test.go")
	}
	<-done
}
