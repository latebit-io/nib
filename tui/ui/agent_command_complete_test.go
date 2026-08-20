package ui

import (
	"context"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// completionRegistry builds a registry with the given command names for
// completion tests, reusing the dispatch-test fakes.
func completionRegistry(t *testing.T, names ...string) *kitcmd.Registry {
	t.Helper()
	reg := kitcmd.NewRegistry()
	for _, n := range names {
		if err := reg.Register(&fakeDispatchHandler{def: builtinDef(n)}); err != nil {
			t.Fatalf("register %q: %v", n, err)
		}
	}
	return reg
}

func TestCommandNamePrefix(t *testing.T) {
	cases := []struct {
		in     string
		prefix string
		ok     bool
	}{
		{"/", "", true},
		{"/ca", "ca", true},
		{"/capabilities", "capabilities", true},
		{"", "", false},
		{"hello", "", false},
		{"/ca ", "", false},      // past the name (space)
		{"/help arg", "", false}, // typing args
		{"/a\nb", "", false},     // multiline
	}
	for _, c := range cases {
		prefix, ok := commandNamePrefix(c.in)
		if ok != c.ok || prefix != c.prefix {
			t.Errorf("commandNamePrefix(%q) = (%q,%v), want (%q,%v)", c.in, prefix, ok, c.prefix, c.ok)
		}
	}
}

func TestCompletion_RefreshFiltersByPrefix(t *testing.T) {
	reg := completionRegistry(t, "help", "clear", "capabilities", "compact")
	var c commandCompletion

	c.refresh("/c", reg)
	if !c.active {
		t.Fatal("expected active for /c")
	}
	got := map[string]bool{}
	for _, m := range c.matches {
		got[m.name] = true
	}
	// "clear", "capabilities", "compact" start with c; "help" does not.
	if !got["clear"] || !got["capabilities"] || !got["compact"] || got["help"] {
		t.Fatalf("unexpected matches: %v", got)
	}
}

func TestCompletion_RefreshDismissCases(t *testing.T) {
	reg := completionRegistry(t, "help", "clear")
	var c commandCompletion

	c.refresh("/", reg)
	if !c.active || len(c.matches) != 2 {
		t.Fatalf("/ should list all commands, got active=%v n=%d", c.active, len(c.matches))
	}
	c.refresh("/help arg", reg) // past the name → dismiss
	if c.active {
		t.Error("expected dismiss once typing args")
	}
	c.refresh("/zzz", reg) // no match → dismiss
	if c.active {
		t.Error("expected dismiss on no match")
	}
	c.refresh("not a command", reg)
	if c.active {
		t.Error("expected dismiss for non-slash input")
	}
}

func TestCompletion_RefreshNilRegistry(t *testing.T) {
	var c commandCompletion
	c.active = true
	c.refresh("/c", nil)
	if c.active {
		t.Error("nil registry should dismiss")
	}
}

// aliasDef builds a command definition with aliases for completion tests.
func aliasDef(name string, aliases ...string) kitcmd.Definition {
	d := builtinDef(name)
	d.Aliases = aliases
	return d
}

func TestCompletion_MatchesAliases(t *testing.T) {
	reg := kitcmd.NewRegistry()
	if err := reg.Register(&fakeDispatchHandler{def: aliasDef("capabilities", "plugins")}); err != nil {
		t.Fatal(err)
	}
	var c commandCompletion

	// Typing toward the alias must surface the command under that alias.
	c.refresh("/plug", reg)
	if !c.active || len(c.matches) != 1 || c.matches[0].name != "plugins" {
		t.Fatalf("/plug should match the plugins alias, got active=%v matches=%+v", c.active, c.matches)
	}
	// Canonical name still matches under its own prefix.
	c.refresh("/cap", reg)
	if !c.active || len(c.matches) != 1 || c.matches[0].name != "capabilities" {
		t.Fatalf("/cap should match capabilities, got %+v", c.matches)
	}
}

func TestCompletion_PreservesSelectionOnSamePrefix(t *testing.T) {
	reg := completionRegistry(t, "alpha", "beta", "gamma")
	var c commandCompletion
	c.refresh("/", reg)
	c.selectNext()
	c.selectNext() // selected = 2 (gamma)
	if c.selected != 2 {
		t.Fatalf("setup: selected = %d, want 2", c.selected)
	}
	// A refresh with the SAME content (e.g. a cursor move) must keep the
	// highlight, not snap back to the top.
	c.refresh("/", reg)
	if c.selected != 2 {
		t.Errorf("same-prefix refresh reset selection to %d, want 2", c.selected)
	}
	// A refresh with a DIFFERENT prefix resets to the top.
	c.refresh("/a", reg)
	if c.selected != 0 {
		t.Errorf("changed-prefix refresh should reset to 0, got %d", c.selected)
	}
}

func TestCompletion_SelectWraps(t *testing.T) {
	reg := completionRegistry(t, "alpha", "beta", "gamma")
	var c commandCompletion
	c.refresh("/", reg)
	if c.selected != 0 {
		t.Fatalf("selected starts at 0, got %d", c.selected)
	}
	c.selectPrev() // wrap to last
	if c.selected != len(c.matches)-1 {
		t.Errorf("selectPrev from 0 should wrap to last, got %d", c.selected)
	}
	c.selectNext() // wrap back to first
	if c.selected != 0 {
		t.Errorf("selectNext should wrap to 0, got %d", c.selected)
	}
}

func TestAcceptCommandCompletion_SetsInput(t *testing.T) {
	m := dispatchTestPane()
	reg := completionRegistry(t, "capabilities", "compact")
	m.SetCommandDispatch(context.Background(), reg, nil)

	m.cmdComplete.refresh("/cap", reg)
	if !m.cmdComplete.active {
		t.Fatal("expected active for /cap")
	}
	// /cap matches only "capabilities".
	if len(m.cmdComplete.matches) != 1 || m.cmdComplete.matches[0].name != "capabilities" {
		t.Fatalf("unexpected matches: %+v", m.cmdComplete.matches)
	}
	m.acceptCommandCompletion()
	if got := m.input.Content(); got != "/capabilities " {
		t.Errorf("input after accept = %q, want %q", got, "/capabilities ")
	}
	if m.cmdComplete.active {
		t.Error("popup should be dismissed after accept")
	}
}

// TestCompletion_PaintsIntoView verifies the popup actually overwrites
// rows in the composed view (the overlay path the logic tests skip).
func TestCompletion_PaintsIntoView(t *testing.T) {
	m := dispatchTestPane()
	m.SetSize(80, 30)
	m.SetInputActive(true)
	reg := completionRegistry(t, "capabilities", "compact", "clear")
	m.SetCommandDispatch(context.Background(), reg, nil)

	m.input.SetContent("/c")
	m.recomputeInputLayout()
	m.cmdComplete.refresh(m.input.Content(), reg)
	if !m.cmdComplete.active {
		t.Fatal("popup should be active for /c")
	}

	view := m.Render()
	if !strings.Contains(view, "/capabilities") {
		t.Error("rendered view should contain the /capabilities completion row")
	}
	if !strings.Contains(view, "/compact") {
		t.Error("rendered view should contain the /compact completion row")
	}
}
