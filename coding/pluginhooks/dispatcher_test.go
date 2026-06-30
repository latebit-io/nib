package pluginhooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/kit/hookrun"
	"github.com/latebit-io/nib/kit/hookspec"
)

// parseCfg parses a hooks.json body into a config, compiling its
// matchers. Tests must go through Parse (not struct literals) because the
// compiled matcher regex is unexported and a literal leaves it nil
// (match-all).
func parseCfg(t *testing.T, body string) hookspec.Config {
	t.Helper()
	c, err := hookspec.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse config: %v\nbody: %s", err, body)
	}
	return c
}

// preToolCfg builds a single-group PreToolUse config with one shell
// command hook.
func preToolCfg(t *testing.T, matcher, shellCmd string) hookspec.Config {
	t.Helper()
	body := fmt.Sprintf(
		`{"hooks":{"PreToolUse":[{"matcher":%q,"hooks":[{"type":"command","command":%q}]}]}}`,
		matcher, shellCmd)
	return parseCfg(t, body)
}

const denyJSON = `echo '{"decision":"deny","reason":"blocked write"}'`

func TestPreToolUse_DenyMatchesViaCCAlias(t *testing.T) {
	// Matcher is the CC name "Write"; the call is the nib name
	// "write_file". hookmap reconciles them, so the deny fires.
	d := New([]hookspec.Config{preToolCfg(t, "Write", denyJSON)}, hookrun.Runner{}, t.TempDir())
	dec := d.PreToolUse(context.Background(), "write_file", `{"path":"x"}`)
	if !dec.Deny || dec.Reason != "blocked write" {
		t.Fatalf("PreToolUse = %+v, want Deny with reason %q", dec, "blocked write")
	}
}

func TestPreToolUse_NoMatchProceeds(t *testing.T) {
	// Matcher "Write" (alias of write_file) must not fire for read_file.
	d := New([]hookspec.Config{preToolCfg(t, "Write", denyJSON)}, hookrun.Runner{}, t.TempDir())
	if dec := d.PreToolUse(context.Background(), "read_file", `{}`); dec.Deny {
		t.Fatalf("PreToolUse(read_file) = %+v, want proceed", dec)
	}
}

func TestPreToolUse_ProceedHook(t *testing.T) {
	d := New([]hookspec.Config{preToolCfg(t, "Bash", "true")}, hookrun.Runner{}, t.TempDir())
	if dec := d.PreToolUse(context.Background(), "bash", `{"command":"ls"}`); dec.Deny {
		t.Fatalf("PreToolUse = %+v, want proceed", dec)
	}
}

func TestPreToolUse_ExitCodeDeny(t *testing.T) {
	// No JSON on stdout: a non-zero exit is CC's deny convention.
	d := New([]hookspec.Config{preToolCfg(t, "Bash", "exit 2")}, hookrun.Runner{}, t.TempDir())
	if dec := d.PreToolUse(context.Background(), "bash", `{}`); !dec.Deny {
		t.Fatalf("PreToolUse = %+v, want Deny on non-zero exit", dec)
	}
}

func TestPreToolUse_FirstDenyWinsAcrossConfigs(t *testing.T) {
	// Config order is the wiring layer's plugin-ID sort; the dispatcher
	// honors it. First config proceeds, second denies.
	d := New([]hookspec.Config{
		preToolCfg(t, "Write", "true"),
		preToolCfg(t, "Write", `echo '{"decision":"deny","reason":"second"}'`),
	}, hookrun.Runner{}, t.TempDir())
	dec := d.PreToolUse(context.Background(), "write_file", `{}`)
	if !dec.Deny || dec.Reason != "second" {
		t.Fatalf("PreToolUse = %+v, want Deny with reason %q", dec, "second")
	}
}

func TestPreToolUse_InfraFailureFailsOpen(t *testing.T) {
	// An argv hook naming a nonexistent binary never spawns; hookrun
	// fails open, so the dispatcher proceeds (never denies on infra error).
	body := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":["definitely-not-a-real-binary-xyz"]}]}]}}`
	d := New([]hookspec.Config{parseCfg(t, body)}, hookrun.Runner{}, t.TempDir())
	if dec := d.PreToolUse(context.Background(), "bash", `{}`); dec.Deny {
		t.Fatalf("PreToolUse = %+v, want proceed (fail open)", dec)
	}
}

func TestPreToolUse_NonRunnableHookSkipped(t *testing.T) {
	// An http hook is parsed but not yet runnable; it must be skipped, not
	// treated as a deny or a panic.
	body := `{"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"http","url":"https://example.test/hook"}]}]}}`
	d := New([]hookspec.Config{parseCfg(t, body)}, hookrun.Runner{}, t.TempDir())
	if dec := d.PreToolUse(context.Background(), "write_file", `{}`); dec.Deny {
		t.Fatalf("PreToolUse = %+v, want proceed (non-runnable skipped)", dec)
	}
}

func TestPostToolUse_DenyMapsToError(t *testing.T) {
	body := fmt.Sprintf(
		`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":%q}]}]}}`,
		`echo '{"decision":"deny","reason":"bad output"}'`)
	d := New([]hookspec.Config{parseCfg(t, body)}, hookrun.Runner{}, t.TempDir())
	content, isErr := d.PostToolUse(context.Background(), "bash", `{}`, "some output")
	if content == nil || *content != "bad output" {
		t.Fatalf("override content = %v, want %q", content, "bad output")
	}
	if isErr == nil || !*isErr {
		t.Fatalf("isError = %v, want true", isErr)
	}
}

func TestPostToolUse_ProceedLeavesResultUntouched(t *testing.T) {
	body := `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"true"}]}]}}`
	d := New([]hookspec.Config{parseCfg(t, body)}, hookrun.Runner{}, t.TempDir())
	content, isErr := d.PostToolUse(context.Background(), "bash", `{}`, "out")
	if content != nil || isErr != nil {
		t.Fatalf("PostToolUse override = (%v, %v), want (nil, nil)", content, isErr)
	}
}

func TestUserPromptSubmit_DenyIgnoresMatcher(t *testing.T) {
	// UserPromptSubmit has no tool; every group fires regardless of an
	// (irrelevant) matcher.
	body := fmt.Sprintf(
		`{"hooks":{"UserPromptSubmit":[{"matcher":"Write","hooks":[{"type":"command","command":%q}]}]}}`,
		`echo '{"decision":"deny","reason":"forbidden prompt"}'`)
	d := New([]hookspec.Config{parseCfg(t, body)}, hookrun.Runner{}, t.TempDir())
	dec := d.UserPromptSubmit(context.Background(), "do the thing")
	if !dec.Deny || dec.Reason != "forbidden prompt" {
		t.Fatalf("UserPromptSubmit = %+v, want Deny with reason %q", dec, "forbidden prompt")
	}
}

func TestLifecycleEvents_FireForSideEffects(t *testing.T) {
	dir := t.TempDir()
	events := map[string]hookspec.Event{
		"SessionStart": hookspec.SessionStart,
		"Stop":         hookspec.Stop,
		"PreCompact":   hookspec.PreCompact,
		"SubagentStop": hookspec.SubagentStop,
	}
	for name, ev := range events {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(dir, name+".marker")
			body := fmt.Sprintf(
				`{"hooks":{%q:[{"matcher":"","hooks":[{"type":"command","command":%q}]}]}}`,
				name, "touch "+marker)
			d := New([]hookspec.Config{parseCfg(t, body)}, hookrun.Runner{}, dir)
			switch ev {
			case hookspec.SessionStart:
				d.SessionStart(context.Background())
			case hookspec.Stop:
				d.Stop(context.Background())
			case hookspec.PreCompact:
				d.PreCompact(context.Background())
			case hookspec.SubagentStop:
				d.SubagentStop(context.Background())
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("%s hook did not run (marker missing): %v", name, err)
			}
		})
	}
}

func TestNilDispatcher_NoOps(t *testing.T) {
	var d *Dispatcher
	if dec := d.PreToolUse(context.Background(), "write_file", `{}`); dec.Deny {
		t.Fatalf("nil PreToolUse = %+v, want proceed", dec)
	}
	if c, e := d.PostToolUse(context.Background(), "bash", `{}`, "x"); c != nil || e != nil {
		t.Fatalf("nil PostToolUse = (%v, %v), want (nil, nil)", c, e)
	}
	if dec := d.UserPromptSubmit(context.Background(), "p"); dec.Deny {
		t.Fatalf("nil UserPromptSubmit = %+v, want proceed", dec)
	}
	// Lifecycle methods must not panic on a nil receiver.
	d.SessionStart(context.Background())
	d.Stop(context.Background())
	d.PreCompact(context.Background())
	d.SubagentStop(context.Background())
}

func TestEmptyConfigs_Proceed(t *testing.T) {
	d := New(nil, hookrun.Runner{}, t.TempDir())
	if dec := d.PreToolUse(context.Background(), "write_file", `{}`); dec.Deny {
		t.Fatalf("empty PreToolUse = %+v, want proceed", dec)
	}
}
