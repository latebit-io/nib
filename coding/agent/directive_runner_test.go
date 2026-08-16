package agent

import (
	"context"
	"strings"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/dyncontext"
)

// shellBinderTool is a minimal kit.ShellBinder extra tool: Execute runs
// one fixed command through whatever runner it was bound with, so a
// test can observe which bash path a directive took.
type shellBinderTool struct {
	command string
	runner  dyncontext.Runner
}

func (s shellBinderTool) Definition() llm.ToolDef {
	return llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: "skill_probe"}}
}

func (s shellBinderTool) Execute(ctx context.Context, _ llm.ToolCall) upagent.ToolResult {
	if s.runner == nil {
		return upagent.ToolResult{Content: "unbound"}
	}
	out, err := s.runner.Run(ctx, s.command)
	if err != nil {
		return upagent.ToolResult{Content: err.Error(), IsError: true}
	}
	return upagent.ToolResult{Content: out}
}

func (s shellBinderTool) BindShell(r dyncontext.Runner) Tool {
	s.runner = r
	return s
}

// recordingBash stands in for the registered bash tool and records the
// synthetic call the runner minted.
type recordingBash struct {
	calls []llm.ToolCall
	res   upagent.ToolResult
}

func (b *recordingBash) Definition() llm.ToolDef {
	return llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: "bash"}}
}

func (b *recordingBash) Execute(_ context.Context, call llm.ToolCall) upagent.ToolResult {
	b.calls = append(b.calls, call)
	return b.res
}

func TestBashToolRunner_IssuesBashCall(t *testing.T) {
	bash := &recordingBash{res: upagent.ToolResult{Content: "main\n"}}
	r := newBashToolRunner(bash)

	out, err := r.Run(context.Background(), "git branch --show-current")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "main\n" {
		t.Fatalf("out = %q, want tool content", out)
	}
	if len(bash.calls) != 1 {
		t.Fatalf("bash calls = %d, want 1", len(bash.calls))
	}
	c := bash.calls[0]
	if c.Function.Name != "bash" || !strings.Contains(c.Function.Arguments, `"command":"git branch --show-current"`) {
		t.Fatalf("unexpected call: %+v", c.Function)
	}
	if !strings.HasPrefix(c.ID, directiveCallPrefix) {
		t.Fatalf("call id %q lacks directive prefix", c.ID)
	}
	// Second directive mints a distinct id.
	if _, err := r.Run(context.Background(), "date"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if bash.calls[1].ID == c.ID {
		t.Fatalf("directive ids must be unique, got %q twice", c.ID)
	}
}

func TestBashToolRunner_ToolErrorBecomesRunError(t *testing.T) {
	bash := &recordingBash{res: upagent.ToolResult{Content: "Command blocked", IsError: true}}
	_, err := newBashToolRunner(bash).Run(context.Background(), "rm -rf /")
	if err == nil || !strings.Contains(err.Error(), "Command blocked") {
		t.Fatalf("err = %v, want tool refusal surfaced as error", err)
	}
}

func TestBashToolRunner_NilToolRefuses(t *testing.T) {
	_, err := newBashToolRunner(nil).Run(context.Background(), "date")
	if err == nil {
		t.Fatal("nil bash tool must refuse directives")
	}
}

// TestRegisterTools_BindsShellToRegisteredBash locks the rebinding at
// registration: a ShellBinder extra tool's directives must flow through
// the agent's own bash gate. Observed via a subagent whose grants deny
// the directive's command — the grant gate, not a raw shell, answers.
func TestRegisterTools_BindsShellToRegisteredBash(t *testing.T) {
	probe := shellBinderTool{command: "rm -rf build"}

	t.Run("subagent grant gate answers directives", func(t *testing.T) {
		ag := New(&multiTurnProvider{}, stubWorkspace{},
			&NewOptions{BuiltinToolGrants: mkMatcher(t, "Bash(git *)", "")}, probe)
		t.Cleanup(ag.Close)
		res := ag.tools["skill_probe"].Execute(context.Background(), llm.ToolCall{})
		if !res.IsError || !strings.Contains(res.Content, "not permitted") {
			t.Fatalf("directive should be denied by the child's grants, got %+v", res)
		}
	})

	t.Run("child without bash refuses directives", func(t *testing.T) {
		ag := New(&multiTurnProvider{}, stubWorkspace{},
			&NewOptions{BuiltinToolGrants: mkMatcher(t, "read_file", "")}, probe)
		t.Cleanup(ag.Close)
		res := ag.tools["skill_probe"].Execute(context.Background(), llm.ToolCall{})
		if !res.IsError || !strings.Contains(res.Content, "not available") {
			t.Fatalf("directive should be refused with no bash registered, got %+v", res)
		}
	})

	t.Run("shared tool value binds per agent", func(t *testing.T) {
		// The same probe value registered in two agents must not share a
		// runner: rebinding returns copies.
		a1 := New(&multiTurnProvider{}, stubWorkspace{},
			&NewOptions{BuiltinToolGrants: mkMatcher(t, "read_file", "")}, probe)
		t.Cleanup(a1.Close)
		a2 := New(&multiTurnProvider{}, stubWorkspace{},
			&NewOptions{BuiltinToolGrants: mkMatcher(t, "Bash(git *)", "")}, probe)
		t.Cleanup(a2.Close)
		r1 := a1.tools["skill_probe"].Execute(context.Background(), llm.ToolCall{})
		r2 := a2.tools["skill_probe"].Execute(context.Background(), llm.ToolCall{})
		if !strings.Contains(r1.Content, "not available") || !strings.Contains(r2.Content, "not permitted") {
			t.Fatalf("agents share a runner: a1=%q a2=%q", r1.Content, r2.Content)
		}
		if probe.runner != nil {
			t.Fatal("registration mutated the caller's tool value")
		}
	})
}
