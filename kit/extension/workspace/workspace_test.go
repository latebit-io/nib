package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/permission"
)

// TestSpec_BuildsShellAndFileTools: the extension is on by default and
// brings the shell, the file tools, search, and its prompt text.
func TestSpec_BuildsShellAndFileTools(t *testing.T) {
	spec := Spec()
	if !spec.Default {
		t.Fatalf("spec = %+v, want it on by default", spec)
	}
	dir := t.TempDir()
	ext, err := spec.New(context.Background(), extension.Env{Dir: dir, Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range ext.Toolset.Tools {
		names = append(names, tl.Definition().Function.Name)
	}
	if got := strings.Join(names, ","); got != "bash,read_file,write_file,edit_file,glob,list_dir,search_project" {
		t.Errorf("tools = %s", got)
	}
	if !strings.Contains(ext.Prompt, dir) {
		t.Errorf("prompt does not name the project: %q", ext.Prompt)
	}
	if err := ext.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestPolicies: the approve mode asks about commands and file changes
// and not about reads; an approved risky command is let through by the
// shell, which refuses the same command in the autonomous mode.
func TestPolicies(t *testing.T) {
	dir := t.TempDir()
	var asked []permission.Request
	answer := permission.Deny
	gate := permission.New()
	gate.SetAsker(func(_ context.Context, r permission.Request) (permission.Decision, error) {
		asked = append(asked, r)
		return answer, nil
	})
	ext, err := Spec().New(context.Background(), extension.Env{Dir: dir, Root: dir, Permission: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ext.Close() }() // read-only handle; nothing to flush
	shell := ext.Toolset.Tools[0]
	// run sends a call through the gate and, if it passes, the shell.
	run := func(id, command string) (blocked bool, output agent.ToolResult) {
		t.Helper()
		args := `{"command":"` + command + `"}`
		res, err := gate.Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: id, Name: "bash", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.Block {
			return true, agent.ToolResult{}
		}
		return false, shell.Execute(context.Background(), llm.ToolCall{ID: id, Function: llm.FunctionCall{Name: "bash", Arguments: args}})
	}

	if blocked, out := run("1", "rm -rf build"); blocked || !out.IsError {
		t.Errorf("autonomous: the gate must pass the call and the shell itself must refuse it: blocked %v, %+v", blocked, out)
	}
	if len(asked) != 0 {
		t.Fatalf("the autonomous mode asked %d times", len(asked))
	}

	gate.SetMode(permission.Approve)
	if blocked, _ := run("2", "rm -rf build"); !blocked {
		t.Error("a denied command must not reach the shell")
	}
	answer = permission.Allow
	if blocked, out := run("3", "rm -rf build"); blocked || out.IsError {
		t.Errorf("an approved risky command must run: blocked %v, %+v", blocked, out)
	}
	if asked[0].Detail != "rm -rf build" || asked[0].Reason == "" {
		t.Errorf("request = %+v, want the command and why it is risky", asked[0])
	}
	before := len(asked)
	for tool, args := range map[string]string{"read_file": `{"path":"a"}`, "glob": `{"pattern":"*"}`, "bash": `{"command":"grep -r x ."}`} {
		res, err := gate.Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: tool, Name: tool, Args: args})
		if err != nil || res.Block {
			t.Errorf("%s: %+v, %v; want it to pass", tool, res, err)
		}
	}
	if len(asked) != before {
		t.Errorf("reads and search commands were asked about %d times", len(asked)-before)
	}
	res, err := gate.Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: "w", Name: "write_file", Args: `{"path":"a.go"}`})
	if err != nil || res.Block || asked[len(asked)-1].Detail != "a.go" {
		t.Errorf("write_file: %+v, %v, last request %+v; want it asked about with its path", res, err, asked[len(asked)-1])
	}
}

// TestShell_WithoutAGateStillRefuses: built without a gate, nothing can
// approve a risky command, so the shell refuses it.
func TestShell_WithoutAGateStillRefuses(t *testing.T) {
	dir := t.TempDir()
	ext, err := Spec().New(context.Background(), extension.Env{Dir: dir, Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ext.Close() }() // read-only handle; nothing to flush
	out := ext.Toolset.Tools[0].Execute(context.Background(), llm.ToolCall{ID: "1", Function: llm.FunctionCall{Arguments: `{"command":"rm -rf build"}`}})
	if !out.IsError {
		t.Errorf("result = %+v, want the shell's refusal", out)
	}
}
