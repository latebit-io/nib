package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/llm"
)

// initRepo creates a git repo in a temp dir with one committed file
// and returns its path. Skips the test when git is unavailable.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Hermetic identity/config: no dependence on the runner's
		// global git config.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "initial commit")
	return dir
}

func toolCall(args map[string]any) llm.ToolCall {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return llm.ToolCall{Function: llm.FunctionCall{Arguments: string(b)}}
}

func TestInRepo(t *testing.T) {
	repo := initRepo(t)
	if !InRepo(repo) {
		t.Fatal("InRepo(repo) = false, want true")
	}
	if InRepo(t.TempDir()) {
		t.Fatal("InRepo(non-repo) = true, want false")
	}
	// Empty root must fail closed, NOT probe the process cwd (which in
	// this test run is nib's own repository and would report true).
	if InRepo("") {
		t.Fatal("InRepo(\"\") = true; empty root probed the process cwd")
	}
}

func TestEmptyArgumentsMeanDefaults(t *testing.T) {
	repo := initRepo(t)
	for _, empty := range []string{"", "   "} {
		call := llm.ToolCall{Function: llm.FunctionCall{Arguments: empty}}
		if res := NewDiffTool(repo).Execute(context.Background(), call); res.IsError {
			t.Fatalf("git_diff with Arguments=%q errored: %s", empty, res.Content)
		}
		if res := NewLogTool(repo).Execute(context.Background(), call); res.IsError {
			t.Fatalf("git_log with Arguments=%q errored: %s", empty, res.Content)
		}
	}
}

func TestTruncateRuneBoundaries(t *testing.T) {
	// Fill the head boundary region with multibyte runes so a naive
	// byte cut would land mid-sequence.
	head := strings.Repeat("界", maxHeadBytes/3+10)
	tail := strings.Repeat("界", maxTailBytes/3+10)
	got := truncate(head + strings.Repeat("m", 8192) + tail)
	if !utf8.ValidString(got) {
		t.Fatal("truncate produced invalid UTF-8")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("oversize multibyte output missing truncation marker")
	}
}

func TestStatusTool(t *testing.T) {
	repo := initRepo(t)
	tool := NewStatusTool(repo)

	t.Run("clean tree", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(nil))
		if res.IsError {
			t.Fatalf("status errored: %s", res.Content)
		}
		if !strings.Contains(res.Content, "## main") || !strings.Contains(res.Content, "clean") {
			t.Fatalf("status = %q, want branch header + clean marker", res.Content)
		}
	})

	t.Run("dirty tree lists the file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := tool.Execute(context.Background(), toolCall(nil))
		if !strings.Contains(res.Content, "a.txt") {
			t.Fatalf("status = %q, want modified a.txt entry", res.Content)
		}
	})

	t.Run("non-repo errors clearly", func(t *testing.T) {
		res := NewStatusTool(t.TempDir()).Execute(context.Background(), toolCall(nil))
		if !res.IsError {
			t.Fatal("status outside a repo must return an error result")
		}
	})
}

func TestDiffTool(t *testing.T) {
	repo := initRepo(t)
	tool := NewDiffTool(repo)

	t.Run("no changes", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(nil))
		if res.IsError || !strings.Contains(res.Content, "no differences") {
			t.Fatalf("clean diff = %+v, want no-differences marker", res)
		}
	})

	t.Run("working-tree change shows in diff", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := tool.Execute(context.Background(), toolCall(nil))
		if !strings.Contains(res.Content, "-one") || !strings.Contains(res.Content, "+two") {
			t.Fatalf("diff = %q, want -one/+two hunks", res.Content)
		}
	})

	t.Run("stat summarizes", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(map[string]any{"stat": true}))
		if strings.Contains(res.Content, "+two") || !strings.Contains(res.Content, "a.txt") {
			t.Fatalf("stat diff = %q, want summary without hunks", res.Content)
		}
	})

	t.Run("path scopes", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(map[string]any{"path": "missing.txt"}))
		if res.IsError || !strings.Contains(res.Content, "no differences") {
			t.Fatalf("scoped diff = %+v, want empty", res)
		}
	})

	t.Run("flag-shaped args rejected", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"ref": "--output=/tmp/x"},
			{"path": "-R"},
		} {
			res := tool.Execute(context.Background(), toolCall(args))
			if !res.IsError || !strings.Contains(res.Content, "must not start with '-'") {
				t.Fatalf("args %v = %+v, want flag-injection rejection", args, res)
			}
		}
	})

	t.Run("bad ref surfaces git's error", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(map[string]any{"ref": "no-such-ref"}))
		if !res.IsError {
			t.Fatal("unknown ref must return an error result")
		}
	})
}

func TestLogTool(t *testing.T) {
	repo := initRepo(t)
	tool := NewLogTool(repo)

	t.Run("shows the commit", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(nil))
		if res.IsError || !strings.Contains(res.Content, "initial commit") {
			t.Fatalf("log = %+v, want the initial commit", res)
		}
	})

	t.Run("count is clamped", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(map[string]any{"count": 100000}))
		if res.IsError {
			t.Fatalf("clamped log errored: %s", res.Content)
		}
	})

	t.Run("path scoping", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(map[string]any{"path": "a.txt"}))
		if res.IsError || !strings.Contains(res.Content, "initial commit") {
			t.Fatalf("scoped log = %+v, want the commit touching a.txt", res)
		}
	})

	t.Run("flag-shaped ref rejected", func(t *testing.T) {
		res := tool.Execute(context.Background(), toolCall(map[string]any{"ref": "--all"}))
		if !res.IsError {
			t.Fatal("flag-shaped ref must be rejected")
		}
	})
}

func TestTruncate(t *testing.T) {
	small := strings.Repeat("x", 100)
	if truncate(small) != small {
		t.Fatal("small output must pass through untouched")
	}
	big := strings.Repeat("a", maxHeadBytes) + strings.Repeat("b", 4096) + strings.Repeat("c", maxTailBytes)
	got := truncate(big)
	if !strings.Contains(got, "truncated") {
		t.Fatal("oversize output missing truncation marker")
	}
	if !strings.HasPrefix(got, "a") || !strings.HasSuffix(got, "c") {
		t.Fatal("truncation must keep head and tail")
	}
	if strings.Contains(got, "bbbb") {
		t.Fatal("middle section must be dropped")
	}
}

func TestValidateArg(t *testing.T) {
	if err := validateArg("ref", "main"); err != nil {
		t.Fatalf("plain ref rejected: %v", err)
	}
	if err := validateArg("ref", "HEAD~1"); err != nil {
		t.Fatalf("tilde ref rejected: %v", err)
	}
	for _, bad := range []string{"-R", "--all", "a\nb", "a\x00b"} {
		if err := validateArg("ref", bad); err == nil {
			t.Fatalf("validateArg(%q) = nil, want error", bad)
		}
	}
}
