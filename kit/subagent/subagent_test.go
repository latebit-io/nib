package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/skill"
	"github.com/latebit-io/nib/kit/tools/files"
)

// tool is a fake tool that records its calls.
func tool(name string, calls *[]string) kit.Tool {
	return agent.NewTool(name, "A "+name+".", func(_ context.Context, args struct {
		Command string `json:"command,omitempty"`
		Path    string `json:"path,omitempty"`
	}) (string, error) {
		*calls = append(*calls, strings.TrimSpace(name+" "+args.Command+args.Path))
		return "ok", nil
	})
}

// host runs children on provider with the tools given.
func host(provider llm.Provider, tools []kit.Tool, stopped *[]string) Host {
	return Host{
		Provider: func(model, effort string) (llm.Provider, error) {
			if model != "" {
				return nil, errors.New("no model " + model)
			}
			return provider, nil
		},
		Tools:  func() []kit.Tool { return tools },
		Hooks:  func(llm.Provider) kit.Hooks { return kit.Hooks{} },
		Prompt: func(persona string) string { return persona + "\n\nproject text" },
		OnStop: func(_ context.Context, kind, answer string) { *stopped = append(*stopped, kind+": "+answer) },
	}
}

func call(args string) llm.ToolCall {
	return llm.ToolCall{ID: "c", Function: llm.FunctionCall{Arguments: args}}
}

// TestAgentTool: the child gets the persona as its system prompt and the
// task as its only message, works with the tools, and its final answer
// comes back; the stop is reported.
func TestAgentTool(t *testing.T) {
	var calls, stopped []string
	provider := llmtest.NewScripted(llmtest.ToolCalls(llmtest.Call("1", "read_file", `{"path":"go.mod"}`)), llmtest.Text("  the module is nib  "))
	tools, errs := AgentTools([]agentdef.Definition{{Name: "reviewer", Description: "Reviews code.", SystemPrompt: "You review code."}},
		host(provider, []kit.Tool{tool("read_file", &calls)}, &stopped))
	if len(errs) != 0 || len(tools) != 1 {
		t.Fatalf("tools %d, errs %v", len(tools), errs)
	}
	def := tools[0].Definition().Function
	if def.Name != "agent_reviewer" || !strings.Contains(def.Description, "Reviews code.") || len(def.Parameters.Required) != 1 {
		t.Errorf("definition = %+v", def)
	}
	res := tools[0].Execute(context.Background(), call(`{"task":"what is the module called?"}`))
	if res.IsError || res.Content != "the module is nib" {
		t.Fatalf("result = %+v", res)
	}
	first := provider.Requests()[0].Messages
	if len(first) != 2 || first[0].Content != "You review code.\n\nproject text" || first[1].Content != "what is the module called?" {
		t.Errorf("child started with %+v", first)
	}
	if len(calls) != 1 || len(stopped) != 1 || stopped[0] != "reviewer: the module is nib" {
		t.Errorf("calls %v, stopped %v", calls, stopped)
	}
	if res := tools[0].Execute(context.Background(), call(`{}`)); !res.IsError {
		t.Error("a missing task must be an error")
	}
}

// TestGrants: a child sees only the tools its definition grants, by
// Claude Code's names too, and an argument-scoped shell grant is checked
// command by command, compound commands refused.
func TestGrants(t *testing.T) {
	var calls, stopped []string
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("1", "bash", `{"command":"git status"}`), llmtest.Call("2", "bash", `{"command":"rm -rf /"}`),
			llmtest.Call("3", "bash", `{"command":"git status; rm x"}`), llmtest.Call("4", "write_file", `{"path":"x"}`)),
		llmtest.Text("done"))
	all := []kit.Tool{tool("bash", &calls), tool("read_file", &calls), tool("write_file", &calls)}
	tools, errs := AgentTools([]agentdef.Definition{{Name: "git", Description: "Git.", AllowedTools: []string{"Read", "Bash(git *)"}}}, host(provider, all, &stopped))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if res := tools[0].Execute(context.Background(), call(`{"task":"look"}`)); res.IsError {
		t.Fatalf("result = %+v", res)
	}
	offered := provider.Requests()[0].Tools
	var names []string
	for _, d := range offered {
		names = append(names, d.Function.Name)
	}
	if strings.Join(names, ",") != "bash,read_file" {
		t.Errorf("child offered %v, want bash and read_file only", names)
	}
	if strings.Join(calls, "|") != "bash git status" {
		t.Errorf("ran %v, want only the granted git command", calls)
	}
}

// TestRefusedDefinitions: a definition asking for a worktree is left
// out; one naming a model the host cannot build fails when called.
func TestRefusedDefinitions(t *testing.T) {
	var stopped []string
	tools, errs := AgentTools([]agentdef.Definition{
		{Name: "isolated", Description: "x", Isolation: agentdef.IsolationWorktree},
		{Name: "big", Description: "x", Model: "huge-model"},
	}, host(llmtest.NewScripted(), nil, &stopped))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "worktree") || len(tools) != 1 {
		t.Fatalf("tools %d, errs %v", len(tools), errs)
	}
	if res := tools[0].Execute(context.Background(), call(`{"task":"x"}`)); !res.IsError || !strings.Contains(res.Content, "huge-model") {
		t.Errorf("result = %+v", res)
	}
}

// TestForkTool: a forked skill's instructions are the child's task, with
// whatever the caller adds; the task argument is optional.
func TestForkTool(t *testing.T) {
	var stopped []string
	provider := llmtest.NewScripted(llmtest.Text("summary"))
	tools, errs := ForkTools([]skill.Skill{{Name: "summarize", Description: "Summarizes.", Body: "Summarize the changes."}}, host(provider, nil, &stopped))
	if len(errs) != 0 || len(tools) != 1 || tools[0].Definition().Function.Name != "skill_summarize" {
		t.Fatalf("tools %v, errs %v", tools, errs)
	}
	if len(tools[0].Definition().Function.Parameters.Required) != 0 {
		t.Error("a fork skill's task must be optional")
	}
	if res := tools[0].Execute(context.Background(), call(`{"task":"only the docs"}`)); res.IsError || res.Content != "summary" {
		t.Fatalf("result = %+v", res)
	}
	if got := provider.Requests()[0].Messages[1].Content; got != "Summarize the changes.\n\nonly the docs" {
		t.Errorf("task = %q", got)
	}
}

// TestDiscover: the project's agents win a name over the person's.
func TestDiscover(t *testing.T) {
	root, global := t.TempDir(), t.TempDir()
	t.Setenv(brand.EnvKeyGlobalAgentsDir, global)
	write := func(dir, name, desc string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + desc + "\n---\nYou help.\n"
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".project", "agents"), "helper", "the project's")
	write(global, "helper", "the person's")
	write(global, "other", "only global")
	defs, errs := Discover(root, nil)
	if len(errs) != 0 || len(defs) != 2 {
		t.Fatalf("defs %+v, errs %v", defs, errs)
	}
	if defs[0].Name != "helper" || defs[0].Description != "the project's" || defs[1].Name != "other" {
		t.Errorf("defs = %+v", defs)
	}
}

// TestGrants_ResolvedPath: a path grant is checked on the file the call
// reaches, not on what it wrote.
func TestGrants_ResolvedPath(t *testing.T) {
	var calls, stopped []string
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("1", "read_file", `{"path":"src/../private.key"}`), llmtest.Call("2", "read_file", `{"path":"src/a.go"}`)),
		llmtest.Text("done"))
	h := host(provider, []kit.Tool{tool("read_file", &calls)}, &stopped)
	tools, errs := AgentTools([]agentdef.Definition{{Name: "src", Description: "x", AllowedTools: []string{"Read(src/*)"}}}, h)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	tools[0].Execute(context.Background(), call(`{"task":"read"}`))
	if strings.Join(calls, "|") != "read_file src/a.go" {
		t.Errorf("read %v; want only src/a.go", calls)
	}
}

// TestPreloadedSkills: the skills a definition names join its persona;
// one the host lacks is reported.
func TestPreloadedSkills(t *testing.T) {
	var stopped []string
	provider := llmtest.NewScripted(llmtest.Text("ok"))
	h := host(provider, nil, &stopped)
	h.Skills = func(names []string) (string, []string) { return "### policy\n\nNever push.", names[1:] }
	tools, errs := AgentTools([]agentdef.Definition{{Name: "p", Description: "x", SystemPrompt: "You help.", Skills: []string{"policy", "gone"}}}, h)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "gone") {
		t.Errorf("errs = %v; want the missing skill named", errs)
	}
	tools[0].Execute(context.Background(), call(`{"task":"x"}`))
	if sys := provider.Requests()[0].Messages[0].Content; !strings.Contains(sys, "You help.\n\n## Skills\n\n### policy\n\nNever push.") {
		t.Errorf("system prompt = %q", sys)
	}
}

// TestGrants_FilePatterns: a file pattern and a call's path are compared
// as the same file: ./x and x alike, and a pattern through a linked
// directory names where it points.
func TestGrants_FilePatterns(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".vault", filepath.Join(root, "secrets")); err != nil {
		t.Fatal(err)
	}
	// An entry literally named "*", pointing out of src.
	if err := os.Mkdir(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "private.key"), filepath.Join(root, "src", "*")); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) (string, error) { return files.Resolve(root, root, p) }
	cases := []struct {
		name        string
		allow, deny []string
		resolve     func(string) (string, error)
		paths       []string
	}{
		{"a ./ deny blocks both spellings", []string{"Read"}, []string{"Read(./private.key)"}, nil, []string{"private.key", "./private.key", "src/a.go"}},
		{"a ./ allow admits plain paths", []string{"Read(./src/*)"}, nil, nil, []string{"src/a.go", "private.key"}},
		{"the same with a resolver", []string{"Read"}, []string{"Read(./private.key)"}, resolve, []string{"./private.key", "src/a.go"}},
		{"a deny through a linked directory", []string{"Read"}, []string{"Read(secrets/*)"}, resolve, []string{".vault/key", "secrets/key", "src/a.go"}},
		{"a wildcard is not an entry", []string{"Read(src/*)"}, nil, resolve, []string{"private.key", "src/a.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls, stopped []string
			var reqs []llm.ToolCall
			for i, p := range tc.paths {
				reqs = append(reqs, llmtest.Call(strconv.Itoa(i), "read_file", fmt.Sprintf(`{"path":%q}`, p)))
			}
			h := host(llmtest.NewScripted(llmtest.ToolCalls(reqs...), llmtest.Text("done")), []kit.Tool{tool("read_file", &calls)}, &stopped)
			h.Resolve = tc.resolve
			tools, errs := AgentTools([]agentdef.Definition{{Name: "a", Description: "x", AllowedTools: tc.allow, DisallowedTools: tc.deny}}, h)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			tools[0].Execute(context.Background(), call(`{"task":"read"}`))
			if got := strings.Join(calls, "|"); got != "read_file src/a.go" {
				t.Errorf("read %q; want only src/a.go", got)
			}
		})
	}
}

// TestGrants_OmittedPath: a call without a path is judged on the working
// directory, which list_dir lists by default.
func TestGrants_OmittedPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		deny []string
		want string
	}{
		{"a deny elsewhere lets it list", []string{"LS(secret/*)"}, "list_dir"},
		{"a deny of everything refuses it", []string{"LS(*)"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, stopped []string
			provider := llmtest.NewScripted(llmtest.ToolCalls(llmtest.Call("1", "list_dir", `{}`)), llmtest.Text("done"))
			h := host(provider, []kit.Tool{tool("list_dir", &calls)}, &stopped)
			root := t.TempDir()
			h.Resolve = func(p string) (string, error) { return files.Resolve(root, root, p) }
			tools, errs := AgentTools([]agentdef.Definition{{Name: "a", Description: "x", DisallowedTools: tc.deny}}, h)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			tools[0].Execute(context.Background(), call(`{"task":"list"}`))
			if got := strings.Join(calls, "|"); got != tc.want {
				t.Errorf("ran %q; want %q", got, tc.want)
			}
		})
	}
}
