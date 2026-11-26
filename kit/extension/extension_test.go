package extension

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/toolperm"
)

func spec(name string, on bool) Spec {
	return Spec{Name: name, Default: on, New: func(context.Context, Env) (Extension, error) {
		return Extension{}, nil
	}}
}

func selected(t *testing.T, r *Registry, s Switches) string {
	t.Helper()
	specs, err := r.Select(s)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ",")
}

// TestSelect: defaults run unless switched off, others only when
// switched on, and switching off wins over switching on.
func TestSelect(t *testing.T) {
	r, err := NewRegistry(spec("files", true), spec("coding", false), spec("web", false))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		enable, disable []string
		want            string
	}{
		{nil, nil, "files"},
		{[]string{"web"}, nil, "files,web"},
		{nil, []string{"files"}, ""},
		{[]string{"coding", "web"}, []string{"web"}, "files,coding"},
	} {
		if got := selected(t, r, Switches{Enable: tc.enable, Disable: tc.disable}); got != tc.want {
			t.Errorf("Select(%v, %v) = %q, want %q", tc.enable, tc.disable, got, tc.want)
		}
	}
	if _, err := r.Select(Switches{Enable: []string{"codng"}}); err == nil || !strings.Contains(err.Error(), "codng") {
		t.Errorf("unknown name: err = %v, want it named", err)
	}
}

func TestNewRegistry_RejectsBadSpecs(t *testing.T) {
	if _, err := NewRegistry(spec("a", true), spec("a", false)); err == nil {
		t.Error("a repeated name must be an error")
	}
	if _, err := NewRegistry(Spec{Name: "nofactory"}); err == nil {
		t.Error("a spec without a factory must be an error")
	}
	var none *Registry
	if got, err := none.Select(Switches{}); err != nil || len(got) != 0 {
		t.Errorf("nil registry: %v, %v; want nothing", got, err)
	}
}

type params struct{}

func named(name string) kit.Tool {
	return agent.NewTool(name, "test tool", func(context.Context, params) (string, error) { return "", nil })
}

// TestRestrict: an allow list keeps only what it grants; a bare deny
// removes a tool with or without one.
func TestRestrict(t *testing.T) {
	ext := Extension{Toolset: kit.Toolset{Tools: []kit.Tool{named("read"), named("write"), named("bash")}}}
	rule := func(s string) toolperm.Rule {
		r, err := toolperm.ParseRule(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	toolNames := func(e Extension) string {
		var out []string
		for _, tl := range e.Toolset.Tools {
			out = append(out, tl.Definition().Function.Name)
		}
		return strings.Join(out, ",")
	}
	if got := toolNames(Restrict(ext, toolperm.New([]toolperm.Rule{rule("read"), rule("bash")}, nil))); got != "read,bash" {
		t.Errorf("allow list: %q, want read,bash", got)
	}
	if got := toolNames(Restrict(ext, toolperm.New(nil, []toolperm.Rule{rule("write")}))); got != "read,bash" {
		t.Errorf("deny only: %q, want read,bash", got)
	}
	if got := toolNames(Restrict(ext, nil)); got != "read,write,bash" {
		t.Errorf("no matcher: %q, want every tool", got)
	}
	if len(ext.Toolset.Tools) != 3 {
		t.Error("Restrict must not change the extension it was given")
	}
}

// TestSwitches_LaterLevelDecides: a flag can switch on what the
// configuration switched off, and the other way round.
func TestSwitches_LaterLevelDecides(t *testing.T) {
	r, err := NewRegistry(spec("workspace", true), spec("coding", false), spec("web", false))
	if err != nil {
		t.Fatal(err)
	}
	config := Switches{Enable: []string{"coding"}, Disable: []string{"workspace"}}
	flags := Switches{Enable: []string{"workspace", "web"}, Disable: []string{"coding"}}
	if got := selected(t, r, config.Then(flags)); got != "workspace,web" {
		t.Errorf("config then flags = %q, want workspace,web", got)
	}
}

func TestLoadSwitches(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadSwitches(filepath.Join(dir, "none.json")); err != nil || len(s.Enable)+len(s.Disable) != 0 {
		t.Errorf("missing file: %+v, %v; want nothing switched", s, err)
	}
	path := filepath.Join(dir, "extensions.json")
	if err := os.WriteFile(path, []byte(`{"enable":["web"],"disable":["workspace"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSwitches(path)
	if err != nil || len(s.Enable) != 1 || s.Enable[0] != "web" || len(s.Disable) != 1 {
		t.Errorf("LoadSwitches = %+v, %v", s, err)
	}
	if err := os.WriteFile(path, []byte(`{nope`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSwitches(path); err == nil {
		t.Error("a malformed file must be an error")
	}
}
