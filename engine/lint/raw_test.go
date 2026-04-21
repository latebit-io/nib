package lint

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRawLinter_clean(t *testing.T) {
	r := &RawLinter{Command: "true"}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected no findings, got %+v", res.Findings)
	}
}

func TestRawLinter_parsesDiagnostics(t *testing.T) {
	r := &RawLinter{Command: `echo "engine/agent/agent.go:42:5: shadowed var"`}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Line != 42 || res.Findings[0].Col != 5 {
		t.Errorf("position parsed wrong: %+v", res.Findings[0])
	}
	if res.Findings[0].Linter != "style lint" {
		t.Errorf("linter stamp missing: %q", res.Findings[0].Linter)
	}
}

func TestRawLinter_nonzeroExitWithOutput(t *testing.T) {
	// Output doesn't match file:line:col, so it becomes an unstructured
	// finding rather than an error — the agent still sees the message.
	r := &RawLinter{Command: `echo "something went sideways"; exit 1`}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 unstructured finding, got %d", len(res.Findings))
	}
	if !strings.Contains(res.Findings[0].Message, "something went sideways") {
		t.Errorf("finding message: %q", res.Findings[0].Message)
	}
}

func TestRawLinter_nonzeroExitNoOutput(t *testing.T) {
	r := &RawLinter{Command: "exit 2"}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error == nil {
		t.Fatalf("expected infrastructure error, got clean result")
	}
	if !strings.Contains(res.Error.Error(), "exit 2") {
		t.Errorf("error should mention exit code: %v", res.Error)
	}
}

func TestRawLinter_cancelOverridesPartialFindings(t *testing.T) {
	// Before the fix: a cancelled-mid-output run that had already written
	// parseable diagnostic lines would return those as "findings," masking
	// the cancellation. The craftsmanship rule is that incomplete data must
	// not be presented as complete — cancellation wins over partial findings.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &RawLinter{
		// Command emits a parseable finding immediately, then hangs. After
		// the parent cancel + SIGKILL, the partial stdout would contain a
		// valid diagnostic line.
		Command: `echo "a.go:1:1: issue"; sleep 30`,
		Timeout: 30 * time.Second,
	}
	res := r.Run(ctx, "", "", nil)
	if res.Error == nil {
		t.Fatal("cancellation must surface as error, not findings")
	}
	if !strings.Contains(res.Error.Error(), "cancel") {
		t.Errorf("error should mention cancellation, got: %v", res.Error)
	}
	if len(res.Findings) != 0 {
		t.Errorf("cancelled run must not expose partial findings, got: %+v", res.Findings)
	}
}

func TestRawLinter_parentCancelled(t *testing.T) {
	// Pre-cancelled parent: the adapter must return promptly with a clear
	// cancellation error, not the generic "exit -1 with no output" signal-kill
	// message. Also verifies no partial findings leak from a killed process.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &RawLinter{
		Command: "sleep 30",
		Timeout: 30 * time.Second, // far longer than the test deadline
	}

	start := time.Now()
	res := r.Run(ctx, "", "", nil)
	elapsed := time.Since(start)

	// exec.Cmd's WaitDelay is 1s, so a killed process can take up to ~1s
	// to reap. Allow some slack for CI.
	if elapsed > 3*time.Second {
		t.Errorf("cancellation should return promptly, took %s", elapsed)
	}
	if res.Error == nil {
		t.Fatal("cancellation should produce an error result")
	}
	if !strings.Contains(res.Error.Error(), "cancel") {
		t.Errorf("error should mention cancellation, got: %v", res.Error)
	}
	if len(res.Findings) != 0 {
		t.Errorf("cancellation must not produce findings, got: %+v", res.Findings)
	}
}

func TestRawLinter_timeout(t *testing.T) {
	r := &RawLinter{
		Command: "sleep 5",
		Timeout: 100 * time.Millisecond,
	}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error == nil || !strings.Contains(res.Error.Error(), "timed out") {
		t.Errorf("expected timeout error, got: %v", res.Error)
	}
}

func TestRawLinter_emptyCommand(t *testing.T) {
	r := &RawLinter{}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error == nil {
		t.Error("empty command should return error")
	}
}

func TestRawLinter_perFileDispatch(t *testing.T) {
	// The {file} placeholder triggers per-file invocation. Command echoes a
	// diagnostic line mentioning the filename; we should see one finding per file.
	r := &RawLinter{Command: `echo "{file}:1:1: issue"`}
	res := r.Run(context.Background(), "", "",
		[]string{"foo.go", "bar.go"})
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 findings (one per file), got %d: %+v", len(res.Findings), res.Findings)
	}
	paths := []string{res.Findings[0].Path, res.Findings[1].Path}
	if paths[0] != "foo.go" || paths[1] != "bar.go" {
		t.Errorf("per-file dispatch wrong: %+v", paths)
	}
}

func TestRawLinter_unsafeDirRejected(t *testing.T) {
	// dir flows directly into `sh -c` via {dir} substitution, so shell
	// metacharacters must be rejected at the adapter boundary. A path like
	// "foo$(whoami)" in an edit proposal would yield dir = "foo$(whoami)"
	// after filepath.Dir.
	r := &RawLinter{Command: `echo "{dir}"`}
	cases := []string{
		"foo$(whoami)",
		"foo; rm -rf /",
		"foo`id`",
		"foo|bar",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			res := r.Run(context.Background(), "", bad, nil)
			if res.Error == nil {
				t.Errorf("expected rejection for dir=%q, got clean result", bad)
			}
			if len(res.Findings) != 0 {
				t.Errorf("expected no findings for unsafe dir, got %v", res.Findings)
			}
		})
	}
}

func TestRawLinter_dirDotAccepted(t *testing.T) {
	// filepath.Dir("root-level.go") returns ".". Must not be rejected.
	r := &RawLinter{Command: "true"}
	res := r.Run(context.Background(), "", ".", nil)
	if res.Error != nil {
		t.Errorf("'.' should be accepted as dir, got error: %v", res.Error)
	}
}

func TestRawLinter_unsafePathSkipped(t *testing.T) {
	r := &RawLinter{Command: `echo "{file}:1:1: issue"`}
	res := r.Run(context.Background(), "", "",
		[]string{"safe.go", "bad; rm -rf /.go"})
	// The unsafe path is skipped, so only one finding appears.
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding (unsafe skipped), got %d: %+v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Path != "safe.go" {
		t.Errorf("wrong path: %q", res.Findings[0].Path)
	}
}

func TestSafeForShell(t *testing.T) {
	safe := []string{"src/main.go", "internal/ui/app.go", "file-name_v2.txt", "a/b/c.rs"}
	for _, s := range safe {
		if !safeForShell(s) {
			t.Errorf("safeForShell(%q) = false, want true", s)
		}
	}
	unsafe := []string{
		"",
		"file name.go",
		"src/';echo pwned",
		"$(whoami).go",
		"file`id`.go",
		"a|b.go",
		"a&b.go",
		"a;b.go",
		"file\nname.go",
		"file\tname.go", // tab — completes whitespace coverage alongside space/newline
		"file\rname.go", // CR — same rationale
		"a>b.go",        // redirect
		"a<b.go",        // redirect
		"a*b.go",        // glob expansion
	}
	for _, s := range unsafe {
		if safeForShell(s) {
			t.Errorf("safeForShell(%q) = true, want false", s)
		}
	}
}
