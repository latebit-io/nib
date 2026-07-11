package bash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want GuardClass
	}{
		{"clean command", "go test ./...", GuardNone},
		{"search bypass", "grep -r foo .", GuardSearch},
		{"redirect write", "echo hi > out.txt", GuardFileWrite},
		{"in-place edit", "sed -i 's/a/b/' main.go", GuardFileWrite},
		{"recursive delete", "rm -rf build", GuardDestructive},
		{"git push", "git push origin main", GuardDestructive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			class, msg := Classify(c.cmd)
			if class != c.want {
				t.Fatalf("Classify(%q) = %v, want %v", c.cmd, class, c.want)
			}
			if (msg == "") != (c.want == GuardNone) {
				t.Fatalf("Classify(%q) msg = %q, inconsistent with class %v", c.cmd, msg, class)
			}
		})
	}
}

func TestGuardClassReason(t *testing.T) {
	if GuardNone.Reason() != "" || GuardSearch.Reason() != "" {
		t.Fatal("none/search classes must have no approval reason (they never reach a prompt)")
	}
	if GuardDestructive.Reason() == "" || GuardFileWrite.Reason() == "" {
		t.Fatal("approval-eligible classes must carry a reason for the prompt")
	}
}

func TestApprovalManaged_RelaxesEligibleClasses(t *testing.T) {
	t.Run("file-write runs when managed", func(t *testing.T) {
		dir := t.TempDir()
		tool := New(dir, ApprovalManaged())
		res := tool.Execute(context.Background(), bashCall("echo hi > out.txt"))
		if res.IsError {
			t.Fatalf("managed file-write blocked: %q", res.Content)
		}
		data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
		if err != nil || !strings.Contains(string(data), "hi") {
			t.Fatalf("approved redirect did not execute: err=%v data=%q", err, data)
		}
	})

	t.Run("file-write still blocks when unmanaged", func(t *testing.T) {
		dir := t.TempDir()
		tool := New(dir)
		res := tool.Execute(context.Background(), bashCall("echo hi > out.txt"))
		if !res.IsError {
			t.Fatal("unmanaged file-write must hard-block")
		}
		if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
			t.Fatal("blocked command executed anyway")
		}
	})

	t.Run("destructive runs when managed", func(t *testing.T) {
		dir := t.TempDir()
		victim := filepath.Join(dir, "scratch")
		if err := os.MkdirAll(filepath.Join(victim, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		tool := New(dir, ApprovalManaged())
		res := tool.Execute(context.Background(), bashCall("rm -rf scratch"))
		if res.IsError {
			t.Fatalf("managed destructive command blocked: %q", res.Content)
		}
		if _, err := os.Stat(victim); !os.IsNotExist(err) {
			t.Fatal("approved rm -rf did not execute")
		}
	})

	t.Run("search blocks in both modes", func(t *testing.T) {
		for _, tool := range []*Tool{New(t.TempDir()), New(t.TempDir(), ApprovalManaged())} {
			res := tool.Execute(context.Background(), bashCall("grep -r foo ."))
			if !res.IsError || !strings.Contains(res.Content, "search_project") {
				t.Fatalf("search bypass not redirected (managed=%v): %+v", tool.approvalManaged, res)
			}
		}
	})
}

func TestPromptGuidelinesVariant(t *testing.T) {
	plain := New(t.TempDir()).PromptGuidelines()
	managed := New(t.TempDir(), ApprovalManaged()).PromptGuidelines()
	if len(plain) != 1 || len(managed) != 1 {
		t.Fatalf("guidelines length: plain=%d managed=%d, want 1 each", len(plain), len(managed))
	}
	if plain[0] == managed[0] {
		t.Fatal("managed mode must describe approval, not blanket prohibition")
	}
	if !strings.Contains(managed[0], "approval") {
		t.Fatalf("managed guidance %q does not mention approval", managed[0])
	}
}
