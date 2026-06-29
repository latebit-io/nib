package agentdef

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgent(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParse_Full(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeAgent(t, dir, "reviewer.md", `---
name: security-reviewer
description: Reviews code for vulns
model: claude-opus-4
effort: high
maxTurns: 20
tools: Read Grep Bash(grep *)
disallowedTools: Write Edit
skills: [security-audit]
isolation: worktree
---
You are a security expert. Find vulnerabilities.
`)
	d, err := Parse(path, SourceProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Name != "security-reviewer" || d.Description != "Reviews code for vulns" {
		t.Errorf("identity wrong: %+v", d)
	}
	if d.Model != "claude-opus-4" || d.Effort != "high" || d.MaxTurns != 20 {
		t.Errorf("config wrong: %+v", d)
	}
	if d.Isolation != IsolationWorktree {
		t.Errorf("isolation = %q", d.Isolation)
	}
	if len(d.Skills) != 1 || d.Skills[0] != "security-audit" {
		t.Errorf("skills = %v", d.Skills)
	}
	if d.SystemPrompt != "You are a security expert. Find vulnerabilities." {
		t.Errorf("system prompt = %q", d.SystemPrompt)
	}
	if d.Source != SourceProject {
		t.Errorf("source = %q", d.Source)
	}

	// Grants compile and enforce (deny wins).
	p := d.Permissions()
	if !p.Allows("Read", "x") || !p.Allows("Bash", "grep foo") {
		t.Errorf("expected read/grep allowed")
	}
	if p.Allows("Write", "x") {
		t.Errorf("Write must be denied")
	}
}

func TestParse_NameFallbackAndMinimal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeAgent(t, dir, "Helper.md", "---\ndescription: helps\n---\nDo helpful things.\n")
	d, err := Parse(path, SourceGlobal)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Name != "helper" { // basename, lowercased
		t.Errorf("name = %q, want helper", d.Name)
	}
	if d.Isolation != IsolationNone || d.MaxTurns != 0 || d.Model != "" {
		t.Errorf("defaults wrong: %+v", d)
	}
}

func TestParse_Rejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"no description":    "---\nname: x\n---\nbody\n",
		"empty prompt":      "---\nname: x\ndescription: d\n---\n\n",
		"bad effort":        "---\ndescription: d\neffort: turbo\n---\nbody\n",
		"bad isolation":     "---\ndescription: d\nisolation: vm\n---\nbody\n",
		"negative maxTurns": "---\ndescription: d\nmaxTurns: -1\n---\nbody\n",
		"malformed grant":   "---\ndescription: d\ndisallowedTools: Bash(rm *\n---\nbody\n",
		"invalid name":      "---\nname: Bad Name!\ndescription: d\n---\nbody\n",
	}
	dir := t.TempDir()
	for label, content := range cases {
		path := writeAgent(t, dir, "a.md", content)
		if _, err := Parse(path, SourceProject); err == nil {
			t.Errorf("%s: expected parse error", label)
		}
	}
}

func TestParse_PermissionsFailClosed(t *testing.T) {
	t.Parallel()
	// A Definition built with a malformed deny must deny-all (defense in
	// depth — Parse already rejects, but Permissions is also safe).
	d := Definition{AllowedTools: []string{"Bash(*)"}, DisallowedTools: []string{"Bash(rm *"}}
	if d.Permissions().Allows("Bash", "rm -rf /") {
		t.Errorf("malformed deny must fail closed")
	}
}

func TestLoadDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeAgent(t, dir, "a.md", "---\ndescription: a\n---\nprompt a\n")
	writeAgent(t, dir, "b.md", "---\ndescription: b\n---\nprompt b\n")
	writeAgent(t, dir, "notes.txt", "ignored")

	defs, err := LoadDir(dir, SourcePlugin)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d defs, want 2", len(defs))
	}
	for _, d := range defs {
		if d.Source != SourcePlugin {
			t.Errorf("source = %q", d.Source)
		}
	}

	// Missing dir is a no-op, not an error.
	got, err := LoadDir(filepath.Join(dir, "nope"), SourceProject)
	if err != nil || got != nil {
		t.Errorf("missing dir should be (nil,nil), got %v %v", got, err)
	}
}

func TestLoadDir_AccumulatesErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeAgent(t, dir, "good.md", "---\ndescription: ok\n---\nprompt\n")
	writeAgent(t, dir, "bad.md", "---\ndescription: d\nisolation: vm\n---\nprompt\n")

	defs, err := LoadDir(dir, SourceProject)
	if err == nil {
		t.Errorf("expected accumulated error for bad.md")
	}
	if len(defs) != 1 || defs[0].Name != "good" {
		t.Errorf("valid def should still load: %+v", defs)
	}
}
