package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/kit/pluginstore"
)

func writePluginFixture(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"`+name+`","version":"1.0.0"}`)
	mustWrite(t, filepath.Join(root, "commands", "hello.md"), "---\ndescription: hi\n---\nHello $ARGUMENTS\n")
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newPluginCmd(t *testing.T) *PluginCommand {
	t.Helper()
	store, err := pluginstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewPlugin(store)
}

func lastDisplay(sess *recordingSession) string {
	if len(sess.displays) == 0 {
		return ""
	}
	return sess.displays[len(sess.displays)-1]
}

func TestPlugin_ListEmpty(t *testing.T) {
	cmd := newPluginCmd(t)
	sess := &recordingSession{}
	if err := cmd.Handle(context.Background(), sess, "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastDisplay(sess), "No plugins installed") {
		t.Errorf("display = %q", lastDisplay(sess))
	}
}

func TestPlugin_InstallThenList(t *testing.T) {
	cmd := newPluginCmd(t)
	fixture := writePluginFixture(t, "demo")
	sess := &recordingSession{}

	if err := cmd.Handle(context.Background(), sess, "install "+fixture); err != nil {
		t.Fatalf("install: %v", err)
	}
	out := lastDisplay(sess)
	if !strings.Contains(out, "Installed demo") || !strings.Contains(out, "1 commands") {
		t.Errorf("install display = %q", out)
	}
	if !strings.Contains(out, "Restart nib") {
		t.Errorf("install should tell the user to restart: %q", out)
	}

	sess2 := &recordingSession{}
	if err := cmd.Handle(context.Background(), sess2, ""); err != nil { // default = list
		t.Fatal(err)
	}
	if !strings.Contains(lastDisplay(sess2), "demo") || !strings.Contains(lastDisplay(sess2), "enabled") {
		t.Errorf("list display = %q", lastDisplay(sess2))
	}
}

func TestPlugin_EnableDisableRemove(t *testing.T) {
	cmd := newPluginCmd(t)
	fixture := writePluginFixture(t, "demo")
	if err := cmd.Handle(context.Background(), &recordingSession{}, "install "+fixture); err != nil {
		t.Fatal(err)
	}

	sess := &recordingSession{}
	if err := cmd.Handle(context.Background(), sess, "disable demo"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !strings.Contains(lastDisplay(sess), "Disabled demo") {
		t.Errorf("disable display = %q", lastDisplay(sess))
	}
	// Disabled plugins drop out of the active set.
	if got := cmd.store.ActivePlugins(); len(got) != 0 {
		t.Errorf("expected no active plugins after disable, got %d", len(got))
	}

	if err := cmd.Handle(context.Background(), &recordingSession{}, "remove demo"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := cmd.store.List(); len(got) != 0 {
		t.Errorf("expected empty store after remove, got %d", len(got))
	}
}

func TestPlugin_UnknownSubcommand(t *testing.T) {
	cmd := newPluginCmd(t)
	if err := cmd.Handle(context.Background(), &recordingSession{}, "frobnicate"); err == nil {
		t.Errorf("expected error for unknown subcommand")
	}
}

func TestParseSource(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		in       string
		wantType pluginstore.SourceType
		wantErr  bool
	}{
		{"./local/path", pluginstore.SourceLocal, false},
		{dir, pluginstore.SourceLocal, false},
		{"owner/repo", pluginstore.SourceGitHub, false},
		{"https://github.com/o/r.git", pluginstore.SourceGit, false},
		{"git@github.com:o/r.git", pluginstore.SourceGit, false},
		{"not-a-real-thing", "", true},
	}
	for _, tc := range cases {
		src, err := parseSource(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseSource(%q) err=%v wantErr=%t", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && src.Type != tc.wantType {
			t.Errorf("parseSource(%q) type=%q want %q", tc.in, src.Type, tc.wantType)
		}
	}
}
