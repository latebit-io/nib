package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/kit/pluginstore"
)

// writeHooks writes a hooks.json with the given body to a fresh temp dir
// and returns its path, as ActivePlugin.HooksConfigPath would point at.
func writeHooks(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// denyWrite is a hooks config whose PreToolUse hook denies Write (the CC
// alias of write_file). The command emits a JSON deny decision.
const denyWrite = `{"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"echo '{\"decision\":\"deny\",\"reason\":\"blocked\"}'"}]}]}}`

// proceedWrite is a hooks config whose PreToolUse hook always proceeds.
const proceedWrite = `{"hooks":{"PreToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"true"}]}]}}`

func TestBuildHookDispatcher_TrustGate(t *testing.T) {
	ctx := context.Background()
	allTrusted := func(string) bool { return true }
	noneTrusted := func(string) bool { return false }

	t.Run("trusted plugin hooks run", func(t *testing.T) {
		d := buildHookDispatcher(
			[]pluginstore.ActivePlugin{{ID: "p1", HooksConfigPath: writeHooks(t, denyWrite)}},
			allTrusted, t.TempDir())
		if d == nil {
			t.Fatal("want non-nil dispatcher for a trusted plugin with hooks")
		}
		if dec := d.PreToolUse(ctx, "write_file", "{}"); !dec.Deny {
			t.Fatalf("PreToolUse = %+v, want deny", dec)
		}
	})

	t.Run("untrusted plugin excluded", func(t *testing.T) {
		d := buildHookDispatcher(
			[]pluginstore.ActivePlugin{{ID: "p1", HooksConfigPath: writeHooks(t, denyWrite)}},
			noneTrusted, t.TempDir())
		if d != nil {
			t.Fatal("an untrusted plugin's hooks must not yield a dispatcher")
		}
	})

	t.Run("trust gate filters per plugin", func(t *testing.T) {
		// Only the trusted plugin's (proceed) hook should run; the
		// untrusted plugin's deny hook must be excluded entirely.
		onlyTrusted := func(id string) bool { return id == "trusted" }
		plugins := []pluginstore.ActivePlugin{
			{ID: "trusted", HooksConfigPath: writeHooks(t, proceedWrite)},
			{ID: "untrusted", HooksConfigPath: writeHooks(t, denyWrite)},
		}
		d := buildHookDispatcher(plugins, onlyTrusted, t.TempDir())
		if d == nil {
			t.Fatal("want a dispatcher from the trusted plugin")
		}
		if dec := d.PreToolUse(ctx, "write_file", "{}"); dec.Deny {
			t.Fatalf("PreToolUse = %+v, want proceed (untrusted deny must not run)", dec)
		}
	})

	t.Run("missing config skipped", func(t *testing.T) {
		d := buildHookDispatcher(
			[]pluginstore.ActivePlugin{{ID: "p1", HooksConfigPath: filepath.Join(t.TempDir(), "nope.json")}},
			allTrusted, t.TempDir())
		if d != nil {
			t.Fatal("a missing hooks.json should be skipped (nil dispatcher)")
		}
	})

	t.Run("malformed config skipped", func(t *testing.T) {
		d := buildHookDispatcher(
			[]pluginstore.ActivePlugin{{ID: "p1", HooksConfigPath: writeHooks(t, "{not json")}},
			allTrusted, t.TempDir())
		if d != nil {
			t.Fatal("a malformed hooks.json should be skipped (nil dispatcher)")
		}
	})

	t.Run("no plugins yields nil", func(t *testing.T) {
		if d := buildHookDispatcher(nil, allTrusted, t.TempDir()); d != nil {
			t.Fatal("no plugins should yield a nil dispatcher")
		}
	})
}
