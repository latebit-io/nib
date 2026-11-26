package nib

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/mcp"
)

type noArgs struct{}

func named(name string) kit.Tool {
	return agent.NewTool(name, "A test tool.", func(context.Context, noArgs) (string, error) { return "", nil })
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project builds a repository with an AGENTS.md at the root and in a
// subdirectory, one skill and an untrusted .mcp.json. It returns the
// subdirectory, with the user-level skill directories isolated.
func project(t *testing.T) (root, sub string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", t.TempDir())

	root = t.TempDir()
	sub = filepath.Join(root, "pkg")
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(root, "AGENTS.md"), "root rules")
	write(t, filepath.Join(sub, "AGENTS.md"), "pkg rules")
	write(t, filepath.Join(root, ".agents", "skills", "notes", "SKILL.md"),
		"---\nname: notes\ndescription: Take notes.\n---\nWrite notes.\n")
	write(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"docs":{"command":"docs-server"}}}`)
	return root, sub
}

func toolNames(ts kit.Toolset) []string {
	out := make([]string, 0, len(ts.Tools))
	for _, t := range ts.Tools {
		out = append(out, t.Definition().Function.Name)
	}
	return out
}

// TestAssemble_Blocks: started in a subdirectory, a session gets the
// host's tools, the default extension, the repository's skills and its
// instruction files, and a manifest that says what was refused.
func TestAssemble_Blocks(t *testing.T) {
	root, sub := project(t)
	closed := 0
	registry, err := extension.NewRegistry(
		extension.Spec{Name: "files", Description: "file tools", Default: true,
			New: func(_ context.Context, env extension.Env) (extension.Extension, error) {
				if env.Dir != sub || env.Root != root {
					t.Errorf("env = %+v, want dir %s and root %s", env, sub, root)
				}
				return extension.Extension{
					Toolset: kit.Toolset{Tools: []kit.Tool{named("read")}},
					Prompt:  "Read before you write.",
					Close:   func() error { closed++; return nil },
				}, nil
			}},
		extension.Spec{Name: "coding", Description: "coding tools",
			New: func(context.Context, extension.Env) (extension.Extension, error) {
				t.Error("an extension that is off must not be built")
				return extension.Extension{}, nil
			}},
	)
	if err != nil {
		t.Fatal(err)
	}

	a, err := Assemble(context.Background(), Options{
		Dir: sub, SystemPrompt: "You are nib.", Extensions: registry,
		Toolset: kit.Toolset{Tools: []kit.Tool{named("host")}},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(a.Warnings) != 1 || !errors.Is(a.Warnings[0], mcp.ErrUntrusted) {
		t.Errorf("warnings = %v, want only the untrusted-servers one", a.Warnings)
	}
	if a.Root != root {
		t.Errorf("Root = %q, want the repository root %q", a.Root, root)
	}
	if got := strings.Join(toolNames(a.Toolset), ","); got != "host,read,skill_notes" {
		t.Errorf("tools = %s, want host,read,skill_notes", got)
	}
	at := func(s string) int { return strings.Index(a.SystemPrompt, s) }
	ext, rootAt, pkgAt := at("Read before you write."), at("root rules"), at("pkg rules")
	if at("You are nib.") != 0 || ext <= 0 || rootAt <= ext || pkgAt <= rootAt {
		t.Errorf("prompt order wrong:\n%s", a.SystemPrompt)
	}
	manifest := kit.RenderPlugins(a.Plugins)
	for _, want := range []string{
		"CONTEXT (2)", "pkg/AGENTS.md", "EXTENSION (2)", "(off) coding tools",
		"[extension:files]", "skill_notes", "mcp__docs", "refused: this project's servers are not trusted",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest lacks %q:\n%s", want, manifest)
		}
	}
	if err := a.Close(); err != nil || closed != 1 {
		t.Errorf("Close = %v, extension closed %d times; want nil and once", err, closed)
	}
	if err := a.Close(); err != nil || closed != 1 {
		t.Errorf("second Close = %v, closed %d times; want nothing to happen", err, closed)
	}
}

// TestAssemble_BadExtension: a name nobody registered stops the
// session; an extension that fails to start is a warning.
func TestAssemble_BadExtension(t *testing.T) {
	_, sub := project(t)
	registry, err := extension.NewRegistry(extension.Spec{Name: "flaky", Default: true,
		New: func(context.Context, extension.Env) (extension.Extension, error) {
			return extension.Extension{}, errors.New("no backend")
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Assemble(context.Background(), Options{Dir: sub, Extensions: registry, Switches: extension.Switches{Enable: []string{"nope"}}}); err == nil {
		t.Error("an unknown extension name must be an error")
	}
	a, err := Assemble(context.Background(), Options{Dir: sub, Extensions: registry})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if len(a.Warnings) != 2 || !strings.Contains(a.Warnings[1].Error(), "flaky") {
		t.Errorf("warnings = %v, want the failed extension", a.Warnings)
	}
	if !strings.Contains(kit.RenderPlugins(a.Plugins), "(off)") {
		t.Error("an extension that failed to start must show as off")
	}
}

func TestAssemble_NeedsDir(t *testing.T) {
	if _, err := Assemble(context.Background(), Options{}); err == nil {
		t.Error("Assemble without a directory must fail")
	}
}

// TestSwitch_KeepsTheOtherBlocks: switching an extension changes the
// tools and the prompt, closes what was switched off, and leaves the
// rest of the session as it was.
func TestSwitch_KeepsTheOtherBlocks(t *testing.T) {
	_, sub := project(t)
	built, closed := 0, 0
	registry, err := extension.NewRegistry(extension.Spec{Name: "files", Description: "file tools", Default: true,
		New: func(context.Context, extension.Env) (extension.Extension, error) {
			built++
			return extension.Extension{
				Toolset: kit.Toolset{Tools: []kit.Tool{named("read")}},
				Prompt:  "Read before you write.",
				Close:   func() error { closed++; return nil },
			}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Assemble(context.Background(), Options{Dir: sub, SystemPrompt: "You are nib.", Extensions: registry})
	if err != nil {
		t.Fatal(err)
	}
	warnings := len(a.Warnings)

	changed, err := a.Switch(context.Background(), extension.Switches{})
	if err != nil || changed || built != 1 {
		t.Errorf("same switches: changed %v, err %v, built %d; want nothing to happen", changed, err, built)
	}
	changed, err = a.Switch(context.Background(), extension.Switches{Disable: []string{"files"}})
	if err != nil || !changed || closed != 1 {
		t.Fatalf("switch off: changed %v, err %v, closed %d", changed, err, closed)
	}
	if got := strings.Join(toolNames(a.Toolset), ","); got != "skill_notes" {
		t.Errorf("tools after switching off = %s, want skill_notes", got)
	}
	if strings.Contains(a.SystemPrompt, "Read before you write.") || !strings.Contains(a.SystemPrompt, "pkg rules") {
		t.Errorf("prompt after switching off:\n%s", a.SystemPrompt)
	}
	if states := a.Extensions(); len(states) != 1 || states[0].On {
		t.Errorf("Extensions = %+v, want files off", states)
	}
	if len(a.Warnings) != warnings {
		t.Errorf("a switch repeated the session's warnings: %v", a.Warnings[warnings:])
	}
	if _, err := a.Switch(context.Background(), extension.Switches{Enable: []string{"nope"}}); err == nil {
		t.Error("an unknown name must be an error")
	}
	if changed, err = a.Switch(context.Background(), extension.Switches{}); err != nil || !changed || built != 2 {
		t.Errorf("switch back on: changed %v, err %v, built %d", changed, err, built)
	}
	if err := a.Close(); err != nil || closed != 2 {
		t.Errorf("Close = %v, closed %d; want the extension closed again", err, closed)
	}
}

func TestProjectRoot(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(leaf); got != leaf {
		t.Errorf("without a repository: ProjectRoot = %q, want the directory itself %q", got, leaf)
	}
	// A worktree's .git is a file, so any entry of that name counts.
	write(t, filepath.Join(root, ".git"), "gitdir: elsewhere")
	if got := ProjectRoot(leaf); got != root {
		t.Errorf("ProjectRoot = %q, want %q", got, root)
	}
}
