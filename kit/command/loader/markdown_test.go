package loader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// writeMD writes a single command markdown file under dir and
// returns its path. Centralised so tests can assert against the
// constructed Source.Path.
func writeMD(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestParse_RejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	content := "---\ndescription: huge.\n---\n" + strings.Repeat("x", maxCommandFileBytes+1)
	path := writeMD(t, dir, "huge.md", content)
	if _, err := Parse(path, kitcmd.SourceProject); err == nil {
		t.Fatal("expected an error for an oversize command file")
	}
}

func TestParse_FullFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeMD(t, dir, "review.md", `---
name: review
description: Review code quality
aliases:
  - rev
  - r
---
Review $1 against the project's standards.
Focus areas: $@
`)
	cmd, err := Parse(path, kitcmd.SourceProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	def := cmd.Definition()
	if def.Name != "review" {
		t.Errorf("Name = %q, want review", def.Name)
	}
	if def.Description != "Review code quality" {
		t.Errorf("Description = %q", def.Description)
	}
	if len(def.Aliases) != 2 || def.Aliases[0] != "rev" || def.Aliases[1] != "r" {
		t.Errorf("Aliases = %v, want [rev r]", def.Aliases)
	}
	if def.Source.Kind != kitcmd.SourceProject {
		t.Errorf("Source.Kind = %v, want SourceProject", def.Source.Kind)
	}
	if def.Source.Path != path {
		t.Errorf("Source.Path = %q, want %q", def.Source.Path, path)
	}

	// Render exercises template substitution end-to-end.
	got, err := cmd.Render("foo.go performance memory")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "Review foo.go against the project's standards.\nFocus areas: foo.go performance memory\n"
	if got != want {
		t.Errorf("Render =\n%q\nwant\n%q", got, want)
	}
}

func TestParse_ToolGrants(t *testing.T) {
	dir := t.TempDir()
	path := writeMD(t, dir, "deploy.md", `---
name: deploy
description: ship it
allowed-tools: Bash(git *) Read
disallowed-tools: Bash(git push *)
---
Deploy.
`)
	cmd, err := Parse(path, kitcmd.SourceProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	def := cmd.Definition()
	if len(def.AllowedTools) == 0 || len(def.DisallowedTools) == 0 {
		t.Fatalf("grants not parsed: allowed=%v disallowed=%v", def.AllowedTools, def.DisallowedTools)
	}
	perm := def.Permissions()
	if !perm.Allows("Bash", "git status") || !perm.Allows("Read", "x") {
		t.Errorf("expected git/read allowed")
	}
	if perm.Allows("Bash", "git push origin") {
		t.Errorf("git push must be denied")
	}
}

func TestParse_RejectsMalformedGrant(t *testing.T) {
	dir := t.TempDir()
	path := writeMD(t, dir, "bad.md", `---
name: bad
description: d
allowed-tools: Bash(*)
disallowed-tools: Bash(rm *
---
body
`)
	if _, err := Parse(path, kitcmd.SourceProject); err == nil {
		t.Errorf("expected parse error for malformed disallowed-tools")
	}
}

func TestParse_FilenameFallbackForName(t *testing.T) {
	// Files without a frontmatter `name:` use the filename basename.
	// Lets users author one-line commands without ceremony.
	dir := t.TempDir()
	path := writeMD(t, dir, "snapshot.md", `---
description: Capture a memory snapshot
---
Save a snapshot of the current conversation.`)
	cmd, err := Parse(path, kitcmd.SourceGlobal)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Definition().Name != "snapshot" {
		t.Errorf("Name = %q, want snapshot", cmd.Definition().Name)
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	// A bare file with no frontmatter is valid — the whole content
	// is the template, name comes from the filename.
	dir := t.TempDir()
	path := writeMD(t, dir, "init.md", "Initialize the project work tree.")
	cmd, err := Parse(path, kitcmd.SourceGlobal)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Definition().Name != "init" {
		t.Errorf("Name = %q, want init", cmd.Definition().Name)
	}
	got, _ := cmd.Render("")
	if got != "Initialize the project work tree." {
		t.Errorf("Render = %q", got)
	}
}

func TestParse_FilenameUppercaseLowered(t *testing.T) {
	// Names are normalised to lowercase to match the registry's
	// case-insensitive lookup. A file named "Review.md" becomes
	// the "review" command.
	dir := t.TempDir()
	path := writeMD(t, dir, "Review.md", "review template")
	cmd, err := Parse(path, kitcmd.SourceProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Definition().Name != "review" {
		t.Errorf("Name = %q, want review (lowercased)", cmd.Definition().Name)
	}
}

func TestParse_InvalidNameRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeMD(t, dir, "bad.md", `---
name: "Has Spaces"
---
template`)
	_, err := Parse(path, kitcmd.SourceProject)
	if err == nil || !strings.Contains(err.Error(), "invalid command name") {
		t.Errorf("expected invalid-name error, got %v", err)
	}
}

func TestParse_MalformedYAMLReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := writeMD(t, dir, "broken.md", `---
name: foo
description: [unclosed
---
body`)
	_, err := Parse(path, kitcmd.SourceProject)
	if err == nil {
		t.Errorf("expected YAML parse error, got nil")
	}
}

func TestParse_FrontmatterWithoutClosingFenceTreatedAsBody(t *testing.T) {
	// Lenient policy: a `---` opener without a closing fence means
	// "no frontmatter, the whole file is body." Saves users from
	// silent breakage when they paste a snippet that opens with `---`
	// for unrelated reasons.
	dir := t.TempDir()
	path := writeMD(t, dir, "weird.md", `---
this looks like frontmatter but never closes
the rest of the file is also body`)
	cmd, err := Parse(path, kitcmd.SourceGlobal)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, _ := cmd.Render("")
	if !strings.Contains(got, "looks like frontmatter") {
		t.Errorf("body should include the un-closed frontmatter; got %q", got)
	}
}

func TestParse_MissingFileIsError(t *testing.T) {
	_, err := Parse(filepath.Join(t.TempDir(), "nonexistent.md"), kitcmd.SourceProject)
	if err == nil {
		t.Errorf("Parse on missing file should error")
	}
}

func TestLoadDir_LoadsAllMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, dir, "a.md", "alpha")
	writeMD(t, dir, "b.md", "beta")
	writeMD(t, dir, "ignored.txt", "not markdown") // wrong ext
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeMD(t, dir, filepath.Join("subdir", "c.md"), "gamma")

	cmds, err := LoadDir(dir, kitcmd.SourceProject)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	got := make(map[string]bool)
	for _, c := range cmds {
		got[c.Definition().Name] = true
	}
	if !got["a"] || !got["b"] {
		t.Errorf("missing expected commands; got %v", got)
	}
	if got["ignored"] || got["c"] {
		t.Errorf("LoadDir should not pick up non-md or subdir files; got %v", got)
	}
}

func TestLoadDir_MissingDirIsNotAnError(t *testing.T) {
	// A user without ~/.config/nib/commands or .nib/commands should
	// see "no commands here," not a startup error.
	cmds, err := LoadDir(filepath.Join(t.TempDir(), "nope"), kitcmd.SourceGlobal)
	if err != nil {
		t.Errorf("missing dir should not error; got %v", err)
	}
	if len(cmds) != 0 {
		t.Errorf("expected 0 commands; got %d", len(cmds))
	}
}

func TestLoadDir_PartialFailuresReturnedAlongsideSuccesses(t *testing.T) {
	// One bad file should not block the others. Caller gets the
	// successes AND an aggregated error to log/inspect.
	dir := t.TempDir()
	writeMD(t, dir, "good.md", "ok")
	writeMD(t, dir, "bad.md", `---
name: "has spaces"
---
nope`)
	cmds, err := LoadDir(dir, kitcmd.SourceProject)
	if err == nil {
		t.Errorf("expected aggregated error from bad.md")
	}
	if len(cmds) != 1 || cmds[0].Definition().Name != "good" {
		var names []string
		for _, c := range cmds {
			names = append(names, c.Definition().Name)
		}
		t.Errorf("expected 1 successful command [good]; got %v", names)
	}
}

func TestLoadDir_RegistersCleanlyViaRegistry(t *testing.T) {
	// End-to-end: a loaded MarkdownCommand registers, dispatches
	// through PromptCommand path, and surfaces its rendered text
	// via Session.SubmitPrompt. Catches drift between loader
	// output and registry expectations.
	dir := t.TempDir()
	writeMD(t, dir, "echo.md", `---
description: Echo args
---
The user said: $@`)
	cmds, err := LoadDir(dir, kitcmd.SourceProject)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(cmds) != 1 {
		t.Fatalf("LoadDir returned %d cmds, want 1", len(cmds))
	}
	r := kitcmd.NewRegistry()
	if err := r.Register(cmds[0]); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess := &recordingSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/echo hello world")
	if !matched || err != nil {
		t.Fatalf("Dispatch matched=%v err=%v", matched, err)
	}
	if len(sess.submits) != 1 || sess.submits[0] != "The user said: hello world" {
		t.Errorf("submits = %v, want [The user said: hello world]", sess.submits)
	}
}

// recordingSession is a [kit/command.Session] for tests that
// captures every Display and SubmitPrompt call.
type recordingSession struct {
	displays []string
	submits  []string
}

func (r *recordingSession) Display(text string) {
	r.displays = append(r.displays, text)
}

func (r *recordingSession) SubmitPrompt(_ context.Context, text string) error {
	r.submits = append(r.submits, text)
	return nil
}

// Compile-time guarantee that recordingSession satisfies kit.Session.
var _ kitcmd.Session = (*recordingSession)(nil)

func TestParse_BOMTolerated(t *testing.T) {
	// Editors on Windows sometimes save files with a UTF-8 BOM.
	// Tolerate the BOM so the user's first-line `---` fence still
	// lines up.
	dir := t.TempDir()
	path := filepath.Join(dir, "review.md")
	body := "\xef\xbb\xbf---\nname: review\n---\nbody"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd, err := Parse(path, kitcmd.SourceProject)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Definition().Name != "review" {
		t.Errorf("Name = %q, want review", cmd.Definition().Name)
	}
	got, _ := cmd.Render("")
	if !strings.Contains(got, "body") {
		t.Errorf("Body should be 'body'; got %q", got)
	}
}

// errCapture is a no-op error sink that confirms LoadDir's
// errors.Join unwrap chain stays intact for callers using
// errors.Is/As.
func TestLoadDir_ErrorIsUnwrappable(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, dir, "bad.md", "---\nname: \"has space\"\n---\nbody")
	_, err := LoadDir(dir, kitcmd.SourceProject)
	if err == nil {
		t.Fatal("expected error")
	}
	// The aggregated error wraps each per-file error verbatim;
	// the substring of the failing path must remain visible for
	// log diagnostics.
	if !strings.Contains(err.Error(), "bad.md") {
		t.Errorf("aggregated error should mention failing file; got %v", err)
	}
	// errors.Is should still see the underlying ErrNotExist or
	// other sentinel — sanity check by wrapping a known error.
	wrapped := errors.Join(errors.New("first"), err)
	if !strings.Contains(wrapped.Error(), "bad.md") {
		t.Errorf("wrapped aggregate should also surface the path")
	}
}
