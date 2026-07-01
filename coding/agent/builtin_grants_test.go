package agent

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/toolperm"
)

// stubTool is a minimal Tool for gating tests: it reports a name and
// records how many times Execute is called.
type stubTool struct {
	name   string
	calls  *int
	result upagent.ToolResult
}

func (s stubTool) Definition() llm.ToolDef {
	return llm.ToolDef{Function: llm.FunctionDef{Name: s.name}}
}

func (s stubTool) Execute(context.Context, llm.ToolCall) upagent.ToolResult {
	if s.calls != nil {
		*s.calls++
	}
	return s.result
}

// mkMatcher builds a nib-keyed matcher from space-separated allow/deny
// grant strings (empty = none).
func mkMatcher(t *testing.T, allow, deny string) *toolperm.Matcher {
	t.Helper()
	a, err := toolperm.ParseField(allow)
	if err != nil {
		t.Fatalf("parse allow %q: %v", allow, err)
	}
	d, err := toolperm.ParseField(deny)
	if err != nil {
		t.Fatalf("parse deny %q: %v", deny, err)
	}
	return toolperm.New(a, d)
}

func TestBuiltinGranted(t *testing.T) {
	cases := []struct {
		name        string
		allow, deny string
		tool        string
		want        bool
	}{
		{"allowlist admits named", "read_file", "", "read_file", true},
		{"allowlist drops unnamed", "read_file", "", "bash", false},
		{"bare deny removes tool", "", "bash", "bash", false},
		{"arg-scoped deny keeps tool", "", "bash(rm *)", "bash", true},
		{"deny-only admits others", "", "write_file", "read_file", true},
		{"deny wins over allow", "bash", "bash", "bash", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := builtinGranted(mkMatcher(t, c.allow, c.deny), c.tool); got != c.want {
				t.Fatalf("builtinGranted(%q) = %v, want %v", c.tool, got, c.want)
			}
		})
	}
}

func TestGateBuiltins_FiltersAndWrapsBash(t *testing.T) {
	builtins := []Tool{
		stubTool{name: "read_file"},
		stubTool{name: "write_file"},
		stubTool{name: "bash"},
	}
	// Grant read_file + a scoped bash; write_file must be dropped.
	out := gateBuiltins(builtins, mkMatcher(t, "read_file bash(git *)", ""))

	var names []string
	var bashWrapped bool
	for _, tl := range out {
		n := tl.Definition().Function.Name
		names = append(names, n)
		if n == "bash" {
			_, bashWrapped = tl.(bashGrantGate)
		}
	}
	slices.Sort(names)
	if want := []string{"bash", "read_file"}; !slices.Equal(names, want) {
		t.Fatalf("gated tools = %v, want %v", names, want)
	}
	if !bashWrapped {
		t.Fatal("bash tool was not wrapped in a bashGrantGate")
	}
}

func TestBashGrantGate_Execute(t *testing.T) {
	grants := mkMatcher(t, "bash(git *)", "")

	t.Run("permitted command delegates", func(t *testing.T) {
		calls := 0
		gate := bashGrantGate{inner: stubTool{name: "bash", calls: &calls, result: upagent.ToolResult{Content: "ok"}}, grants: grants}
		res := gate.Execute(context.Background(), call(`{"command":"git status"}`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1", calls)
		}
		if res.IsError || res.Content != "ok" {
			t.Fatalf("res = %+v, want delegated ok", res)
		}
	})

	t.Run("blocked command does not delegate", func(t *testing.T) {
		calls := 0
		gate := bashGrantGate{inner: stubTool{name: "bash", calls: &calls}, grants: grants}
		res := gate.Execute(context.Background(), call(`{"command":"rm -rf /"}`))
		if calls != 0 {
			t.Fatalf("inner Execute called %d times, want 0 (blocked)", calls)
		}
		if !res.IsError {
			t.Fatalf("res = %+v, want IsError for blocked command", res)
		}
	})

	t.Run("unparseable args delegate to inner", func(t *testing.T) {
		calls := 0
		gate := bashGrantGate{inner: stubTool{name: "bash", calls: &calls}, grants: grants}
		gate.Execute(context.Background(), call(`{not json`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1 (forwarded)", calls)
		}
	})
}

func TestBashGrantGate_CompoundCommandsBlocked(t *testing.T) {
	t.Run("scoped allow blocks command chaining", func(t *testing.T) {
		// `Bash(git *)` must not let a second command ride past the glob.
		gate := bashGrantGate{inner: stubTool{name: "bash"}, grants: mkMatcher(t, "bash(git *)", "")}
		for _, cmd := range []string{
			"git status; rm -rf /",
			"git status && rm -rf /",
			"git status || rm -rf /",
			"git status & rm -rf /",
			"git log | tee /etc/passwd",
			"git status\nrm -rf /",
			"git $(rm -rf /)",
			"git `rm -rf /`",
		} {
			calls := 0
			gate.inner = stubTool{name: "bash", calls: &calls}
			res := gate.Execute(context.Background(), call(`{"command":`+quote(cmd)+`}`))
			if !res.IsError {
				t.Errorf("compound command %q was permitted, want blocked", cmd)
			}
			if calls != 0 {
				t.Errorf("compound command %q reached inner bash", cmd)
			}
		}
	})

	t.Run("scoped deny blocks chaining that evades the deny", func(t *testing.T) {
		// `disallowedTools: Bash(rm *)` must reject `true; rm -rf /`.
		gate := bashGrantGate{inner: stubTool{name: "bash"}, grants: mkMatcher(t, "", "bash(rm *)")}
		res := gate.Execute(context.Background(), call(`{"command":"true; rm -rf /"}`))
		if !res.IsError {
			t.Fatal("chained command evaded a scoped deny, want blocked")
		}
	})

	t.Run("bare bash grant permits compound commands", func(t *testing.T) {
		// A bare `Bash` grant imposes no arg rules — unrestricted.
		calls := 0
		gate := bashGrantGate{inner: stubTool{name: "bash", calls: &calls}, grants: mkMatcher(t, "bash", "")}
		res := gate.Execute(context.Background(), call(`{"command":"git status; ls"}`))
		if res.IsError || calls != 1 {
			t.Fatalf("bare bash grant should permit compound command; res=%+v calls=%d", res, calls)
		}
	})
}

// quote JSON-encodes a string for embedding in a tool-call arguments body.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func call(args string) llm.ToolCall {
	return llm.ToolCall{Function: llm.FunctionCall{Name: "bash", Arguments: args}}
}

func TestNew_BuiltinToolGrants_RestrictsToolSet(t *testing.T) {
	// A child granted only read_file must register read_file and not
	// bash / write_file. Nil grants (control) keeps the full set.
	restricted := New(&multiTurnProvider{}, stubWorkspace{},
		&NewOptions{BuiltinToolGrants: mkMatcher(t, "read_file", "")})
	t.Cleanup(restricted.Close)

	have := toolSet(restricted)
	if !have["read_file"] {
		t.Error("granted read_file missing from restricted agent")
	}
	if have["bash"] || have["write_file"] {
		t.Errorf("ungranted tools present: bash=%v write_file=%v", have["bash"], have["write_file"])
	}

	full := New(&multiTurnProvider{}, stubWorkspace{}, nil)
	t.Cleanup(full.Close)
	if !toolSet(full)["bash"] {
		t.Error("nil grants should keep the full builtin set (bash missing)")
	}
}

func toolSet(ag *Agent) map[string]bool {
	m := make(map[string]bool)
	for _, t := range ag.Tools() {
		m[t.Definition().Function.Name] = true
	}
	return m
}
