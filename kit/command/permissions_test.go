package command

import "testing"

func TestDefinition_PermissionsFailClosed(t *testing.T) {
	t.Parallel()
	// Broad allow plus malformed deny must not leave the allow active.
	def := Definition{
		AllowedTools:    []string{"Bash(*)"},
		DisallowedTools: []string{"Bash(rm *"}, // unbalanced paren
	}
	if def.Permissions().Allows("Bash", "rm -rf /") {
		t.Errorf("malformed deny must fail closed, not drop and let the allow win")
	}
}

func TestDefinition_PermissionsValid(t *testing.T) {
	t.Parallel()
	def := Definition{
		AllowedTools:    []string{"Bash(git *)", "Read"},
		DisallowedTools: []string{"Bash(git push *)"},
	}
	p := def.Permissions()
	if !p.Allows("Bash", "git status") || !p.Allows("Read", "x") {
		t.Errorf("expected git/read allowed")
	}
	if p.Allows("Bash", "git push origin") {
		t.Errorf("git push must be denied")
	}
}
