package dyncontext

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/toolperm"
)

// fakeRunner records commands and returns a canned output/error.
type fakeRunner struct {
	ran []string
	out string
	err error
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (string, error) {
	f.ran = append(f.ran, cmd)
	return f.out, f.err
}

func matcher(t *testing.T, allow, deny string) *toolperm.Matcher {
	t.Helper()
	a, err := toolperm.ParseField(allow)
	if err != nil {
		t.Fatal(err)
	}
	d, err := toolperm.ParseField(deny)
	if err != nil {
		t.Fatal(err)
	}
	return toolperm.New(a, d)
}

func TestExpand_AllowedRuns(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "on branch main"}
	body := "Status: !`git status`\nDone."
	got := Expand(context.Background(), body, matcher(t, "Bash(git *)", ""), r)
	if !strings.Contains(got, "on branch main") {
		t.Errorf("allowed directive should inline output: %q", got)
	}
	if len(r.ran) != 1 || r.ran[0] != "git status" {
		t.Errorf("expected git status run, got %v", r.ran)
	}
}

func TestExpand_DeniedNotRun(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "SHOULD NOT APPEAR"}
	// Grant only git; an rm directive must be blocked and never executed.
	got := Expand(context.Background(), "Clean: !`rm -rf /`", matcher(t, "Bash(git *)", ""), r)
	if strings.Contains(got, "SHOULD NOT APPEAR") {
		t.Errorf("denied directive must not run: %q", got)
	}
	if !strings.Contains(got, "[blocked:") {
		t.Errorf("denied directive should show a blocked marker: %q", got)
	}
	if len(r.ran) != 0 {
		t.Errorf("runner must not be called for a denied directive: %v", r.ran)
	}
}

func TestExpand_DenyWins(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "pushed"}
	perm := matcher(t, "Bash(git *)", "Bash(git push *)")
	got := Expand(context.Background(), "!`git push origin main`", perm, r)
	if strings.Contains(got, "pushed") || !strings.Contains(got, "[blocked:") {
		t.Errorf("disallowed git push must be blocked: %q", got)
	}
}

func TestExpand_FencedBlock(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "v1.2.3"}
	body := "Version:\n```!\nnpm run version\n```\nend"
	got := Expand(context.Background(), body, matcher(t, "Bash(npm *)", ""), r)
	if !strings.Contains(got, "v1.2.3") {
		t.Errorf("fenced block output should inline: %q", got)
	}
	if len(r.ran) != 1 || r.ran[0] != "npm run version" {
		t.Errorf("fenced command not run as expected: %v", r.ran)
	}
}

func TestExpand_NilMatcherFailsClosed(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "nope"}
	got := Expand(context.Background(), "!`git status`", nil, r)
	if len(r.ran) != 0 || !strings.Contains(got, "[blocked:") {
		t.Errorf("nil matcher must deny everything: %q ran=%v", got, r.ran)
	}
}

func TestExpand_RunErrorMarker(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "boom", err: errors.New("exit 1")}
	got := Expand(context.Background(), "!`git status`", matcher(t, "Bash(git *)", ""), r)
	if !strings.Contains(got, "[error running") {
		t.Errorf("run error should surface a marker: %q", got)
	}
}

func TestExpand_SkipsInsideCodeFence(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "RAN"}
	// An inline directive inside a normal documentation fence must stay
	// literal, not execute.
	body := "Example:\n```\n!`git status`\n```\nend"
	got := Expand(context.Background(), body, matcher(t, "Bash(git *)", ""), r)
	if len(r.ran) != 0 {
		t.Errorf("directive inside a code fence must not run: %v", r.ran)
	}
	if !strings.Contains(got, "!`git status`") {
		t.Errorf("fenced directive should be preserved literally:\n%s", got)
	}
}

func TestExpand_BlocksShellChaining(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: "NOPE"}
	// Even with a broad grant, shell chaining is blocked so the grant
	// glob cannot be escaped.
	for _, cmd := range []string{"git status; rm -rf /", "git x && rm y", "echo $(whoami)", "cat a | sh"} {
		got := Expand(context.Background(), "!`"+cmd+"`", matcher(t, "Bash(*)", ""), r)
		if !strings.Contains(got, "[blocked:") {
			t.Errorf("expected %q blocked, got %q", cmd, got)
		}
	}
	if len(r.ran) != 0 {
		t.Errorf("no chained command should run: %v", r.ran)
	}
}

// TestShellRunner_TimeoutKillsChildren proves a backgrounded child that
// holds stdout cannot defeat the timeout, and that a zero-value runner
// does not fire instantly.
func TestShellRunner_TimeoutKillsChildren(t *testing.T) {
	t.Parallel()
	start := time.Now()
	_, err := NewShellRunner(100*time.Millisecond).Run(context.Background(), "sleep 5 & sleep 5")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run took %v; timeout did not kill the process group", d)
	}

	out, err := (ShellRunner{}).Run(context.Background(), "printf ok")
	if err != nil || out != "ok" {
		t.Fatalf("zero-value ShellRunner: out=%q err=%v", out, err)
	}
	if _, err := NewShellRunner(time.Second).Run(context.Background(), "exit 3"); err == nil {
		t.Fatal("non-zero exit must surface as an error")
	}
}

func TestExpand_NoDirectivesUnchanged(t *testing.T) {
	t.Parallel()
	body := "Plain skill body with no directives."
	if got := Expand(context.Background(), body, matcher(t, "", ""), &fakeRunner{}); got != body {
		t.Errorf("directive-free body should pass through unchanged: %q", got)
	}
	if HasDirectives(body) {
		t.Errorf("HasDirectives false positive")
	}
	if !HasDirectives("x !`y` z") {
		t.Errorf("HasDirectives should detect inline directive")
	}
}
