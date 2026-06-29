package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/toolperm"
)

// recordingRunner implements dyncontext.Runner for Execute tests.
type recordingRunner struct {
	ran []string
	out string
}

func (r *recordingRunner) Run(_ context.Context, cmd string) (string, error) {
	r.ran = append(r.ran, cmd)
	return r.out, nil
}

func mustMatcher(t *testing.T, allow string) *toolperm.Matcher {
	t.Helper()
	a, err := toolperm.ParseField(allow)
	if err != nil {
		t.Fatal(err)
	}
	return toolperm.New(a, nil)
}

func TestSkillTool_Execute_GatesDynamicContext(t *testing.T) {
	t.Parallel()

	// Granted: the directive runs and its output is inlined.
	r := &recordingRunner{out: "on branch main"}
	granted := skillTool{
		def:    llm.ToolDef{},
		body:   "Status: !`git status`",
		perm:   mustMatcher(t, "Bash(git *)"),
		runner: r,
	}
	res := granted.Execute(context.Background(), llm.ToolCall{})
	if !strings.Contains(res.Content, "on branch main") {
		t.Errorf("granted directive should inline output: %q", res.Content)
	}
	if len(r.ran) != 1 {
		t.Errorf("expected the command to run once, got %v", r.ran)
	}

	// Prompt-only skill (no Bash grant): the directive is blocked, never run.
	r2 := &recordingRunner{out: "NOPE"}
	promptOnly := skillTool{
		def:    llm.ToolDef{},
		body:   "Status: !`git status`",
		perm:   toolperm.DenyAll(),
		runner: r2,
	}
	res2 := promptOnly.Execute(context.Background(), llm.ToolCall{})
	if strings.Contains(res2.Content, "NOPE") || !strings.Contains(res2.Content, "[blocked:") {
		t.Errorf("prompt-only skill must block the directive: %q", res2.Content)
	}
	if len(r2.ran) != 0 {
		t.Errorf("blocked directive must not run, got %v", r2.ran)
	}
}

func TestSkillTool_Execute_PassthroughWhenNoDirectives(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{out: "x"}
	st := skillTool{body: "Plain body, no directives.", perm: mustMatcher(t, "Bash(git *)"), runner: r}
	res := st.Execute(context.Background(), llm.ToolCall{})
	if res.Content != "Plain body, no directives." {
		t.Errorf("verbatim body expected, got %q", res.Content)
	}
	if len(r.ran) != 0 {
		t.Errorf("no command should run for a directive-free body")
	}
}
