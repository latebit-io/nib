package toolperm

import (
	"reflect"
	"testing"
)

func TestParseRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    Rule
		wantErr bool
	}{
		{"Read", Rule{Tool: "Read"}, false},
		{"  Edit  ", Rule{Tool: "Edit"}, false},
		{"Bash(git *)", Rule{Tool: "Bash", Arg: "git *"}, false},
		{"Write(/etc/*)", Rule{Tool: "Write", Arg: "/etc/*"}, false},
		{"Bash( npm run build )", Rule{Tool: "Bash", Arg: "npm run build"}, false},
		{"", Rule{}, true},
		{"Bash(git *", Rule{}, true},
		{"(x)", Rule{}, true},
		{"Bash()", Rule{}, true},   // empty parens must not mean "any arg"
		{"Bash(  )", Rule{}, true}, // whitespace-only too
	}
	for _, tc := range cases {
		got, err := ParseRule(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseRule(%q) err=%v wantErr=%t", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseRule(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseField(t *testing.T) {
	t.Parallel()
	t.Run("string with paren-spanning args", func(t *testing.T) {
		t.Parallel()
		got, err := ParseField("Bash(git commit -m *) Read, Write(/x)")
		if err != nil {
			t.Fatal(err)
		}
		want := []Rule{
			{Tool: "Bash", Arg: "git commit -m *"},
			{Tool: "Read"},
			{Tool: "Write", Arg: "/x"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("list of strings", func(t *testing.T) {
		t.Parallel()
		got, err := ParseField([]any{"Bash(git *)", "Read"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Tool != "Bash" || got[1].Tool != "Read" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("nil is empty", func(t *testing.T) {
		t.Parallel()
		got, err := ParseField(nil)
		if err != nil || got != nil {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("invalid rule collected", func(t *testing.T) {
		t.Parallel()
		got, err := ParseField("Read Bash(oops")
		if err == nil {
			t.Errorf("expected error for unbalanced paren")
		}
		if len(got) != 1 || got[0].Tool != "Read" {
			t.Errorf("valid rule should still parse: %+v", got)
		}
	})
}

func TestMatcher_Allows(t *testing.T) {
	t.Parallel()
	allow, _ := ParseField("Bash(git *) Read")
	deny, _ := ParseField("Bash(git push *)")
	m := New(allow, deny)

	cases := []struct {
		tool, arg string
		want      bool
	}{
		{"Read", "anything", true},
		{"read", "case-insensitive tool", true}, // tool match is case-insensitive
		{"Bash", "git status", true},
		{"Bash", "git commit -m wip", true},
		{"Bash", "git push origin main", false}, // deny wins over allow
		{"Bash", "rm -rf /", false},             // not in allow
		{"Write", "/tmp/x", false},              // tool not granted
	}
	for _, tc := range cases {
		if got := m.Allows(tc.tool, tc.arg); got != tc.want {
			t.Errorf("Allows(%q,%q) = %t, want %t", tc.tool, tc.arg, got, tc.want)
		}
	}
}

func TestMatcher_PermitsArg(t *testing.T) {
	t.Parallel()

	t.Run("allowlist scopes the tool", func(t *testing.T) {
		// `tools: Bash(git *)` — bash is admitted only for git commands.
		allow, _ := ParseField("Bash(git *)")
		m := New(allow, nil)
		cases := []struct {
			arg  string
			want bool
		}{
			{"git status", true},
			{"git push origin main", true},
			{"rm -rf /", false}, // named in allow ⇒ must match a bash rule
		}
		for _, c := range cases {
			if got := m.PermitsArg("bash", c.arg); got != c.want {
				t.Errorf("PermitsArg(bash, %q) = %t, want %t", c.arg, got, c.want)
			}
		}
	})

	t.Run("deny-only grant blocks only matching commands", func(t *testing.T) {
		// `disallowedTools: Bash(rm *)` with no allow list — bash is
		// unrestricted except for the denied pattern. Allows would wrongly
		// deny everything here (empty allow list); PermitsArg does not.
		deny, _ := ParseField("Bash(rm *)")
		m := New(nil, deny)
		if !m.PermitsArg("bash", "git status") {
			t.Error("deny-only grant must permit a non-denied command")
		}
		if m.PermitsArg("bash", "rm -rf /") {
			t.Error("deny-only grant must block the denied command")
		}
		// Contrast with Allows: the empty allow list denies everything.
		if m.Allows("bash", "git status") {
			t.Error("Allows must deny under an empty allow list (allowlist semantics)")
		}
	})

	t.Run("tool not named is permitted (deny-aware)", func(t *testing.T) {
		// An allow list that names other tools imposes no arg restriction
		// on bash, but a bash deny still blocks.
		allow, _ := ParseField("Read")
		deny, _ := ParseField("Bash(sudo *)")
		m := New(allow, deny)
		if !m.PermitsArg("bash", "ls") {
			t.Error("tool absent from allow list should be permitted")
		}
		if m.PermitsArg("bash", "sudo rm") {
			t.Error("deny rule must still block")
		}
	})
}

func TestMatcher_EmptyAllowDeniesAll(t *testing.T) {
	t.Parallel()
	m := New(nil, nil)
	if m.HasAllowList() {
		t.Errorf("empty matcher should report no allow list")
	}
	if m.Allows("Read", "") {
		t.Errorf("empty allow set must deny")
	}
}

func TestGrantsAndDeniesTool(t *testing.T) {
	t.Parallel()
	allow, _ := ParseField("Read Bash(git *)")
	deny, _ := ParseField("Write Bash(rm *)")
	m := New(allow, deny)

	if !m.GrantsTool("Read") || !m.GrantsTool("bash") {
		t.Errorf("Read and Bash should be granted (tool-level)")
	}
	if m.GrantsTool("Edit") {
		t.Errorf("Edit is not granted")
	}
	if !m.DeniesTool("Write") {
		t.Errorf("bare Write deny should deny the tool")
	}
	// An argument-scoped deny does not remove the tool itself.
	if m.DeniesTool("Bash") {
		t.Errorf("Bash(rm *) must not deny the Bash tool outright")
	}
}

func TestDenyAll(t *testing.T) {
	t.Parallel()
	m := DenyAll()
	if m.Allows("Read", "x") || m.Allows("Bash", "git status") {
		t.Errorf("DenyAll must permit nothing")
	}
}

func TestGlobMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"git *", "git status", true},
		{"git *", "gitx status", false},
		{"*", "anything at all", true},
		{"/etc/*", "/etc/passwd", true},
		{"/etc/*", "/usr/bin", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"npm run *", "npm run build --prod", true},
		{"git *", "git a/b/c", true}, // * crosses '/'
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q,%q) = %t, want %t", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestAnyShell(t *testing.T) {
	t.Parallel()
	shell, _ := ParseField("Read Bash(git *)")
	if !AnyShell(shell) {
		t.Errorf("expected shell grant detected")
	}
	prompt, _ := ParseField("Read Write(/x)")
	if AnyShell(prompt) {
		t.Errorf("prompt-only grants should not register as shell")
	}
}
