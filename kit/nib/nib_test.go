package nib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/meter"
	"github.com/latebit-io/nib/kit/pluginstore"
)

type noArgs struct{}

func named(name string) kit.Tool {
	return agent.NewTool(name, "A test tool.", func(context.Context, noArgs) (string, error) { return "", nil })
}

// slash is a command that does nothing, for manifest tests.
type slash string

func (s slash) Definition() command.Definition {
	return command.Definition{Name: string(s), Description: "A test command."}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project builds a repository with an AGENTS.md at the root and in a
// subdirectory, one skill and an untrusted .mcp.json. It returns the
// subdirectory, with the user-level skill directories isolated.
func project(t *testing.T) (root, sub string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", t.TempDir())

	root = t.TempDir()
	sub = filepath.Join(root, "pkg")
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(root, "AGENTS.md"), "root rules")
	write(t, filepath.Join(sub, "AGENTS.md"), "pkg rules")
	write(t, filepath.Join(root, ".agents", "skills", "notes", "SKILL.md"),
		"---\nname: notes\ndescription: Take notes.\n---\nWrite notes.\n")
	write(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"docs":{"command":"docs-server"}}}`)
	return root, sub
}

func toolNames(ts kit.Toolset) []string {
	out := make([]string, 0, len(ts.Tools))
	for _, t := range ts.Tools {
		out = append(out, t.Definition().Function.Name)
	}
	return out
}

// TestAssemble_Blocks: started in a subdirectory, a session gets the
// host's tools, the default extension, the repository's skills and its
// instruction files, and a manifest that says what was refused.
func TestAssemble_Blocks(t *testing.T) {
	root, sub := project(t)
	closed := 0
	registry, err := extension.NewRegistry(
		extension.Spec{Name: "files", Description: "file tools", Default: true,
			New: func(_ context.Context, env extension.Env) (extension.Extension, error) {
				if env.Dir != sub || env.Root != root {
					t.Errorf("env = %+v, want dir %s and root %s", env, sub, root)
				}
				return extension.Extension{
					Toolset: kit.Toolset{Tools: []kit.Tool{named("read")}, Commands: []command.Command{slash("scan")}},
					Prompt:  "Read before you write.",
					Close:   func() error { closed++; return nil },
				}, nil
			}},
		extension.Spec{Name: "coding", Description: "coding tools",
			New: func(context.Context, extension.Env) (extension.Extension, error) {
				t.Error("an extension that is off must not be built")
				return extension.Extension{}, nil
			}},
	)
	if err != nil {
		t.Fatal(err)
	}

	a, err := Assemble(context.Background(), Options{
		Dir: sub, SystemPrompt: "You are nib.", Extensions: registry,
		Toolset: kit.Toolset{Tools: []kit.Tool{named("host")}, Commands: []command.Command{slash("deploy")}},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(a.Warnings) != 1 || !errors.Is(a.Warnings[0], mcp.ErrUntrusted) {
		t.Errorf("warnings = %v, want only the untrusted-servers one", a.Warnings)
	}
	if a.Root != root {
		t.Errorf("Root = %q, want the repository root %q", a.Root, root)
	}
	if got := strings.Join(toolNames(a.Toolset), ","); got != "host,read,skill_notes" {
		t.Errorf("tools = %s, want host,read,skill_notes", got)
	}
	at := func(s string) int { return strings.Index(a.SystemPrompt, s) }
	ext, rootAt, pkgAt := at("Read before you write."), at("root rules"), at("pkg rules")
	if at("You are nib.") != 0 || ext <= 0 || rootAt <= ext || pkgAt <= rootAt {
		t.Errorf("prompt order wrong:\n%s", a.SystemPrompt)
	}
	manifest := kit.RenderPlugins(a.Plugins)
	for _, want := range []string{
		"CONTEXT (2)", "pkg/AGENTS.md", "EXTENSION (2)", "(off) coding tools",
		"[extension:files]", "skill_notes", "mcp__docs", "refused: this project's servers are not trusted",
		"COMMAND (2)", "/deploy", "/scan",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest lacks %q:\n%s", want, manifest)
		}
	}
	if err := a.Close(); err != nil || closed != 1 {
		t.Errorf("Close = %v, extension closed %d times; want nil and once", err, closed)
	}
	if err := a.Close(); err != nil || closed != 1 {
		t.Errorf("second Close = %v, closed %d times; want nothing to happen", err, closed)
	}
}

// TestAssemble_BadExtension: a name nobody registered stops the
// session; an extension that fails to start is a warning.
func TestAssemble_BadExtension(t *testing.T) {
	_, sub := project(t)
	registry, err := extension.NewRegistry(extension.Spec{Name: "flaky", Default: true,
		New: func(context.Context, extension.Env) (extension.Extension, error) {
			return extension.Extension{}, errors.New("no backend")
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Assemble(context.Background(), Options{Dir: sub, Extensions: registry, Switches: extension.Switches{Enable: []string{"nope"}}}); err == nil {
		t.Error("an unknown extension name must be an error")
	}
	a, err := Assemble(context.Background(), Options{Dir: sub, Extensions: registry})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if len(a.Warnings) != 2 || !strings.Contains(a.Warnings[1].Error(), "flaky") {
		t.Errorf("warnings = %v, want the failed extension", a.Warnings)
	}
	if !strings.Contains(kit.RenderPlugins(a.Plugins), "(off)") {
		t.Error("an extension that failed to start must show as off")
	}
}

func TestAssemble_NeedsDir(t *testing.T) {
	if _, err := Assemble(context.Background(), Options{}); err == nil {
		t.Error("Assemble without a directory must fail")
	}
}

// TestSwitch_KeepsTheOtherBlocks: switching an extension changes the
// tools and the prompt, closes what was switched off, and leaves the
// rest of the session as it was.
func TestSwitch_KeepsTheOtherBlocks(t *testing.T) {
	_, sub := project(t)
	built, closed := 0, 0
	registry, err := extension.NewRegistry(extension.Spec{Name: "files", Description: "file tools", Default: true,
		New: func(context.Context, extension.Env) (extension.Extension, error) {
			built++
			return extension.Extension{
				Toolset: kit.Toolset{Tools: []kit.Tool{named("read")}},
				Prompt:  "Read before you write.",
				Close:   func() error { closed++; return nil },
			}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Assemble(context.Background(), Options{Dir: sub, SystemPrompt: "You are nib.", Extensions: registry})
	if err != nil {
		t.Fatal(err)
	}
	warnings := len(a.Warnings)

	changed, err := a.Switch(context.Background(), extension.Switches{})
	if err != nil || changed || built != 1 {
		t.Errorf("same switches: changed %v, err %v, built %d; want nothing to happen", changed, err, built)
	}
	changed, err = a.Switch(context.Background(), extension.Switches{Disable: []string{"files"}})
	if err != nil || !changed || closed != 1 {
		t.Fatalf("switch off: changed %v, err %v, closed %d", changed, err, closed)
	}
	if got := strings.Join(toolNames(a.Toolset), ","); got != "skill_notes" {
		t.Errorf("tools after switching off = %s, want skill_notes", got)
	}
	if strings.Contains(a.SystemPrompt, "Read before you write.") || !strings.Contains(a.SystemPrompt, "pkg rules") {
		t.Errorf("prompt after switching off:\n%s", a.SystemPrompt)
	}
	if states := a.Extensions(); len(states) != 1 || states[0].On {
		t.Errorf("Extensions = %+v, want files off", states)
	}
	if len(a.Warnings) != warnings {
		t.Errorf("a switch repeated the session's warnings: %v", a.Warnings[warnings:])
	}
	if _, err := a.Switch(context.Background(), extension.Switches{Enable: []string{"nope"}}); err == nil {
		t.Error("an unknown name must be an error")
	}
	if changed, err = a.Switch(context.Background(), extension.Switches{}); err != nil || !changed || built != 2 {
		t.Errorf("switch back on: changed %v, err %v, built %d", changed, err, built)
	}
	if err := a.Close(); err != nil || closed != 2 {
		t.Errorf("Close = %v, closed %d; want the extension closed again", err, closed)
	}
}

func TestProjectRoot(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(leaf); got != leaf {
		t.Errorf("without a repository: ProjectRoot = %q, want the directory itself %q", got, leaf)
	}
	// A worktree's .git is a file, so any entry of that name counts.
	write(t, filepath.Join(root, ".git"), "gitdir: elsewhere")
	if got := ProjectRoot(leaf); got != root {
		t.Errorf("ProjectRoot = %q, want %q", got, root)
	}
}

// TestAgent_BudgetEndsTheRun: once the session has used its token
// budget, the next request is refused and the run ends with that error.
func TestAgent_BudgetEndsTheRun(t *testing.T) {
	_, sub := project(t)
	a, err := Assemble(context.Background(), Options{Dir: sub, Limits: Limits{TokenBudget: 50}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }() // nothing was started that can fail to stop
	spend := &llm.Usage{PromptTokens: 40, CompletionTokens: 20}
	provider := llmtest.NewScripted(llmtest.TextUsage(spend, "first"), llmtest.Text("never sent"))
	ag, err := a.Agent(provider)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	user := func(text string) []llm.Message { return []llm.Message{{Role: "user", Content: text}} }
	if _, err := ag.Run(context.Background(), user("one")); err != nil {
		t.Fatalf("first run under the budget: %v", err)
	}
	if _, err := ag.Run(context.Background(), user("two")); !errors.Is(err, meter.ErrBudgetExceeded) {
		t.Errorf("run past the budget: err = %v, want ErrBudgetExceeded", err)
	}
	if provider.Calls() != 1 || a.Usage().TotalPromptTokens != 40 {
		t.Errorf("provider called %d times, usage %+v; want one call and its usage counted", provider.Calls(), a.Usage())
	}
}

// TestAgent_Delegates: a project's agent definition becomes a tool; the
// child runs on the session's model and budget, with the session's tools
// but not the delegation tools, and its answer reaches the parent.
func TestAgent_Delegates(t *testing.T) {
	root, sub := project(t)
	t.Setenv("NIB_GLOBAL_AGENTS_DIR", t.TempDir())
	write(t, filepath.Join(root, ".project", "agents", "helper.md"), "---\nname: helper\ndescription: Helps.\n---\nYou help.\n")
	a, err := Assemble(context.Background(), Options{Dir: sub, Toolset: kit.Toolset{Tools: []kit.Tool{named("host_tool")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }() // nothing was started that can fail to stop
	if !strings.Contains(kit.RenderPlugins(a.Plugins), "agent_helper") {
		t.Errorf("manifest lacks the agent:\n%s", kit.RenderPlugins(a.Plugins))
	}
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("p1", "agent_helper", `{"task":"say hi"}`)),
		llmtest.TextUsage(&llm.Usage{PromptTokens: 7}, "hi from the child"),
		llmtest.Text("the child said hi"),
	)
	ag, err := a.Agent(provider)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	res, err := ag.Run(context.Background(), []llm.Message{{Role: "user", Content: "ask the helper"}})
	if err != nil || res.Done.FinalText != "the child said hi" {
		t.Fatalf("run = %+v, %v", res.Done, err)
	}
	reqs := provider.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want parent, child, parent", len(reqs))
	}
	child := reqs[1]
	if child.Messages[0].Role != "system" || !strings.HasPrefix(child.Messages[0].Content, "You help.") || strings.Contains(child.Messages[0].Content, "agent_helper") {
		t.Errorf("child system prompt = %q", child.Messages[0].Content)
	}
	var names []string
	for _, d := range child.Tools {
		names = append(names, d.Function.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "host_tool") || strings.Contains(got, "agent_") {
		t.Errorf("child tools = %s; want the session's, without delegation", got)
	}
	if !strings.Contains(reqs[2].Messages[len(reqs[2].Messages)-1].Content, "hi from the child") {
		t.Errorf("the parent did not get the child's answer: %+v", reqs[2].Messages)
	}
	if a.Usage().Turns != 3 || a.Usage().TotalPromptTokens != 7 {
		t.Errorf("usage = %+v; want the child's request counted in the session", a.Usage())
	}
}

// TestAssemble_Plugins: an installed plugin's skills load at once; its
// hooks and agents only once it is trusted.
func TestAssemble_Plugins(t *testing.T) {
	_, sub := project(t)
	t.Setenv("NIB_GLOBAL_AGENTS_DIR", t.TempDir())
	src := t.TempDir()
	write(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"kit"}`)
	write(t, filepath.Join(src, "skills", "tidy", "SKILL.md"), "---\nname: tidy\ndescription: Tidy up.\n---\nTidy.\n")
	write(t, filepath.Join(src, "agents", "scout.md"), "---\nname: scout\ndescription: Scouts.\n---\nYou scout.\n")
	write(t, filepath.Join(src, "hooks", "hooks.json"), `{"hooks":{"PreToolUse":[{"matcher":"host_tool","hooks":[{"type":"command","command":"echo nope >&2; exit 2"}]}]}}`)
	store, err := pluginstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Install(context.Background(), pluginstore.LocalSource(src), pluginstore.InstallOptions{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	assemble := func() *Assembly {
		t.Helper()
		a, err := Assemble(context.Background(), Options{Dir: sub, Plugins: store, Toolset: kit.Toolset{Tools: []kit.Tool{named("host_tool")}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() }) // nothing was started that can fail to stop
		return a
	}
	blocked := func(a *Assembly) bool {
		res, err := a.Toolset.Hooks.BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: "1", Name: "host_tool", Args: "{}"})
		return err == nil && res.Block
	}

	a := assemble()
	tools := strings.Join(toolNames(a.Toolset), ",")
	if !strings.Contains(tools, "skill_kit-tidy") || strings.Contains(tools, "agent_kit-scout") || blocked(a) {
		t.Errorf("untrusted: tools %s, blocked %v; want the skill only", tools, blocked(a))
	}
	if !strings.Contains(fmt.Sprint(a.Warnings), "not trusted") {
		t.Errorf("warnings = %v; want the untrusted plugin named", a.Warnings)
	}

	if err := store.SetTrusted(p.ID, true); err != nil {
		t.Fatal(err)
	}
	a = assemble()
	if tools := strings.Join(toolNames(a.Toolset), ","); !strings.Contains(tools, "agent_kit-scout") || !blocked(a) {
		t.Errorf("trusted: tools %s, blocked %v; want the agent and the hook", tools, blocked(a))
	}
}
