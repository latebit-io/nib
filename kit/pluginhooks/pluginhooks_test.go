package pluginhooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/hookspec"
	"github.com/latebit-io/nib/kit/pluginstore"
)

// dispatcher builds a dispatcher from one hooks file.
func dispatcher(t *testing.T, hooks string) *Dispatcher {
	t.Helper()
	cfg, err := hookspec.Parse([]byte(hooks))
	if err != nil {
		t.Fatal(err)
	}
	return New(t.TempDir(), "s1", []Source{{Plugin: "p", Config: cfg}})
}

// TestToolHooks: a Claude Code matcher reaches nib's tool by its alias,
// exit 2 blocks with stderr as the reason, and context a hook adds
// reaches the model with the result.
func TestToolHooks(t *testing.T) {
	d := dispatcher(t, `{"hooks":{
		"PreToolUse":[
			{"matcher":"Write","hooks":[{"type":"command","command":"echo 'no writes today' >&2; exit 2"}]},
			{"matcher":"Bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"additionalContext\":\"bash ran under a hook\"}}'"}]}
		],
		"PostToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"jq -r .tool_input.file_path >/dev/null 2>&1; echo '{\"decision\":\"block\",\"reason\":\"secrets inside\"}'"}]}]
	}}`)
	h := d.Hooks()
	ctx := context.Background()
	res, err := h.BeforeToolCall(ctx, kit.BeforeToolCallInput{CallID: "1", Name: "write_file", Args: `{"path":"a"}`})
	if err != nil || !res.Block || !strings.Contains(res.Reason, "no writes today") {
		t.Errorf("write = %+v, %v; want it blocked with the hook's reason", res, err)
	}
	if res, _ := h.BeforeToolCall(ctx, kit.BeforeToolCallInput{CallID: "2", Name: "bash", Args: `{"command":"ls"}`}); res.Block {
		t.Errorf("bash blocked: %+v", res)
	}
	after, err := h.AfterToolCall(ctx, kit.AfterToolCallInput{CallID: "2", Name: "bash", Result: agent.ToolResult{Content: "a\nb"}})
	if err != nil || after.Content == nil || *after.Content != "a\nb\n\nbash ran under a hook" {
		t.Errorf("after bash = %+v, %v; want the context added", after, err)
	}
	_, _ = h.BeforeToolCall(ctx, kit.BeforeToolCallInput{CallID: "3", Name: "read_file", Args: `{"path":"x"}`}) // lets the call run
	read, err := h.AfterToolCall(ctx, kit.AfterToolCallInput{CallID: "3", Name: "read_file", Args: `{"path":"x"}`, Result: agent.ToolResult{Content: "key=1"}})
	if err != nil || read.Content == nil || !strings.Contains(*read.Content, "secrets inside") {
		t.Errorf("after read = %+v, %v; want the objection added", read, err)
	}
	if res, _ := h.BeforeToolCall(ctx, kit.BeforeToolCallInput{CallID: "4", Name: "glob"}); res.Block {
		t.Errorf("an unmatched tool was blocked: %+v", res)
	}
}

// TestToolHooks_StrongestWins: hooks run together; a deny beats an
// allow, whatever the order.
func TestToolHooks_StrongestWins(t *testing.T) {
	d := dispatcher(t, `{"hooks":{"PreToolUse":[
		{"matcher":"*","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"allow\"}}'"}]},
		{"matcher":"Bash","hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"not here\"}}'"}]}
	]}}`)
	res, err := d.Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: "1", Name: "bash"})
	if err != nil || !res.Block || !strings.Contains(res.Reason, "not here") {
		t.Errorf("result = %+v, %v; want the deny", res, err)
	}
}

// TestPromptAndLifecycle: a prompt hook adds context or blocks; a stop
// hook that blocks asks the agent to go on; session start adds context.
func TestPromptAndLifecycle(t *testing.T) {
	d := dispatcher(t, `{"hooks":{
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"jq -e '.prompt | test(\"secret\")' >/dev/null && { echo 'no secrets' >&2; exit 2; }; echo 'today is Thursday'"}]}],
		"Stop":[{"hooks":[{"type":"command","command":"jq -e '.stop_hook_active' >/dev/null && exit 0; echo '{\"decision\":\"block\",\"reason\":\"run the tests first\"}'"}]}],
		"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"printf 'branch main'"}]}]
	}}`)
	ctx := context.Background()
	got, err := d.Prompt(ctx, "hello")
	if err != nil || got != "hello\n\ntoday is Thursday" {
		t.Errorf("Prompt = %q, %v", got, err)
	}
	if _, err := d.Prompt(ctx, "my secret"); !errors.As(err, new(*BlockedError)) || !strings.Contains(err.Error(), "no secrets") {
		t.Errorf("blocked prompt = %v", err)
	}
	if again := d.Stop(ctx, "done", false); again != "run the tests first" {
		t.Errorf("Stop = %q, want the hook's reason to go on with", again)
	}
	if again := d.Stop(ctx, "done", true); again != "" {
		t.Errorf("Stop while already going on = %q, want none", again)
	}
	if text := d.SessionStart(ctx, "startup"); text != "branch main" {
		t.Errorf("SessionStart = %q", text)
	}
}

func TestNilDispatcher(t *testing.T) {
	var d *Dispatcher
	if d.Has(hookspec.Stop) || d.Hooks().BeforeToolCall != nil {
		t.Error("a nil dispatcher must run nothing")
	}
	if got, err := d.Prompt(context.Background(), "x"); got != "x" || err != nil {
		t.Errorf("Prompt = %q, %v", got, err)
	}
}

// TestFromPlugins: only a trusted plugin's hooks load.
func TestFromPlugins(t *testing.T) {
	src := t.TempDir()
	for path, body := range map[string]string{
		".claude-plugin/plugin.json": `{"name":"guard"}`,
		"hooks/hooks.json":           `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo \"$CLAUDE_PLUGIN_ROOT\" >&2; exit 2"}]}]}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := pluginstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Install(context.Background(), pluginstore.LocalSource(src), pluginstore.InstallOptions{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	sources, errs := FromPlugins(store, store.ActivePlugins())
	if len(sources) != 0 || len(errs) != 0 {
		t.Fatalf("untrusted: %d sources, %v; want none", len(sources), errs)
	}
	if err := store.SetTrusted(p.ID, true); err != nil {
		t.Fatal(err)
	}
	sources, errs = FromPlugins(store, store.ActivePlugins())
	if len(sources) != 1 || len(errs) != 0 {
		t.Fatalf("trusted: %d sources, %v", len(sources), errs)
	}
	res, _ := New(t.TempDir(), "s", sources).Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: "1", Name: "bash"})
	if !res.Block || !strings.Contains(res.Reason, store.ConvertedDir(p.ID)) {
		t.Errorf("result = %+v; want the plugin's hook to block, seeing its root", res)
	}
}

// TestStopAndBlockedCalls: a hook that ends the work does not send the
// agent back; a call PreToolUse blocked gets no PostToolUse.
func TestStopAndBlockedCalls(t *testing.T) {
	d := dispatcher(t, `{"hooks":{
		"Stop":[{"hooks":[{"type":"command","command":"echo '{\"continue\":false,\"stopReason\":\"enough\"}'"}]}],
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"exit 2"}]}],
		"PostToolUse":[{"hooks":[{"type":"command","command":"echo '{\"hookSpecificOutput\":{\"additionalContext\":\"post ran\"}}'"}]}]
	}}`)
	ctx := context.Background()
	if again := d.Stop(ctx, "done", false); again != "" {
		t.Errorf("Stop = %q; a hook that ends the work must not send the agent back", again)
	}
	h := d.Hooks()
	if res, _ := h.BeforeToolCall(ctx, kit.BeforeToolCallInput{CallID: "b", Name: "bash"}); !res.Block {
		t.Fatal("bash must be blocked")
	}
	if after, _ := h.AfterToolCall(ctx, kit.AfterToolCallInput{CallID: "b", Name: "bash"}); after.Content != nil {
		t.Errorf("PostToolUse ran for a blocked call: %q", *after.Content)
	}
	_, _ = h.BeforeToolCall(ctx, kit.BeforeToolCallInput{CallID: "g", Name: "glob"}) // lets the call run
	if after, _ := h.AfterToolCall(ctx, kit.AfterToolCallInput{CallID: "g", Name: "glob"}); after.Content == nil || !strings.Contains(*after.Content, "post ran") {
		t.Errorf("PostToolUse did not run for a call that ran: %+v", after)
	}
}

// TestLifecycleMatchers: a SessionStart group runs only for its source,
// a SubagentStop group only for its agent type.
func TestLifecycleMatchers(t *testing.T) {
	d := dispatcher(t, `{"hooks":{"SessionStart":[
		{"matcher":"resume","hooks":[{"type":"command","command":"printf resumed"}]},
		{"matcher":"startup","hooks":[{"type":"command","command":"printf started"}]},
		{"hooks":[{"type":"command","command":"printf always"}]}
	]}}`)
	if got := d.SessionStart(context.Background(), "startup"); got != "started\nalways" {
		t.Errorf("SessionStart(startup) = %q; want only the startup group and the unconditional one", got)
	}
}

// TestCCInput: file_path is the path the tool reads, whatever else the
// call carries; other tools pass through.
func TestCCInput(t *testing.T) {
	if got := string(ccInput("read_file", `{"path":"secret.txt","file_path":"public.txt"}`)); !strings.Contains(got, `"file_path":"secret.txt"`) {
		t.Errorf("read_file input = %s; want file_path to be the path that is read", got)
	}
	if got := string(ccInput("mcp__x__y", `{"path":"a"}`)); got != `{"path":"a"}` {
		t.Errorf("mcp input = %s; want it untouched", got)
	}
}
