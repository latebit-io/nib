package files

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/tools/search"
)

// workspace builds root/pkg with a few files and returns tools whose
// working directory is root/pkg.
func workspace(t *testing.T) (root string, tools map[string]agent.Tool) {
	t.Helper()
	root = t.TempDir()
	for name, body := range map[string]string{
		"README.md":         "top\n",
		"pkg/a.go":          "package pkg\n\nfunc A() {}\nfunc B() {}\n",
		"pkg/deep/b.go":     "package deep\n",
		"pkg/deep/notes.md": "notes\n",
		".git/config":       "[core]\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ft, err := New(root, filepath.Join(root, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ft.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	tools = map[string]agent.Tool{}
	for _, tl := range ft.Tools() {
		tools[tl.Definition().Function.Name] = tl
	}
	return root, tools
}

// run calls a tool with args and returns its result.
func run(t *testing.T, tool agent.Tool, args map[string]any) agent.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Arguments: string(raw)}})
}

// ok runs a tool and fails the test on an error result.
func ok(t *testing.T, tool agent.Tool, args map[string]any) string {
	t.Helper()
	res := run(t, tool, args)
	if res.IsError {
		t.Fatalf("%s(%v): %s", tool.Definition().Function.Name, args, res.Content)
	}
	return res.Content
}

func TestRead_NumbersAndPages(t *testing.T) {
	_, tools := workspace(t)
	got := ok(t, tools["read_file"], map[string]any{"path": "a.go"})
	if !strings.Contains(got, "     1\tpackage pkg\n") || !strings.Contains(got, "     4\tfunc B() {}\n") {
		t.Errorf("read = %q, want numbered lines", got)
	}
	got = ok(t, tools["read_file"], map[string]any{"path": "a.go", "offset": 3, "limit": 1})
	if !strings.HasPrefix(got, "     3\tfunc A() {}\n") || !strings.Contains(got, "continue with offset 4") {
		t.Errorf("paged read = %q, want line 3 and where to continue", got)
	}
	if res := run(t, tools["read_file"], map[string]any{"path": "a.go", "offset": 99}); !res.IsError {
		t.Error("an offset past the end must be an error")
	}
}

// TestPaths_ConfinedToRoot: relative paths resolve against the working
// directory, the rest of the project is reachable, and nothing outside is.
func TestPaths_ConfinedToRoot(t *testing.T) {
	root, tools := workspace(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("hunter2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "pkg", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if got := ok(t, tools["read_file"], map[string]any{"path": "../README.md"}); !strings.Contains(got, "top") {
		t.Errorf("a path inside the project above the working directory must be readable, got %q", got)
	}
	if got := ok(t, tools["read_file"], map[string]any{"path": filepath.Join(root, "README.md")}); !strings.Contains(got, "top") {
		t.Errorf("an absolute path inside the project must be readable, got %q", got)
	}
	for _, path := range []string{"../../etc/passwd", outside, "link.txt"} {
		if res := run(t, tools["read_file"], map[string]any{"path": path}); !res.IsError || strings.Contains(res.Content, "hunter2") {
			t.Errorf("read %s: %+v, want a refusal", path, res)
		}
		if res := run(t, tools["write_file"], map[string]any{"path": path, "content": "x"}); !res.IsError {
			t.Errorf("write %s must be refused", path)
		}
	}
	if data, _ := os.ReadFile(outside); string(data) != "hunter2" {
		t.Errorf("the file outside the project changed: %q", data)
	}
}

func TestWrite_CreatesParents(t *testing.T) {
	root, tools := workspace(t)
	ok(t, tools["write_file"], map[string]any{"path": "new/dir/c.go", "content": "package c\n"})
	data, err := os.ReadFile(filepath.Join(root, "pkg", "new", "dir", "c.go"))
	if err != nil || string(data) != "package c\n" {
		t.Errorf("file = %q, %v; want the written content", data, err)
	}
	if res := run(t, tools["write_file"], map[string]any{"path": "big", "content": strings.Repeat("x", maxWriteBytes+1)}); !res.IsError {
		t.Error("content over the cap must be refused")
	}
}

// TestEdit_ExactAndUnique: an edit applies only when the text is found
// exactly once, or everywhere when asked.
func TestEdit_ExactAndUnique(t *testing.T) {
	root, tools := workspace(t)
	file := filepath.Join(root, "pkg", "a.go")
	content := func() string {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ok(t, tools["edit_file"], map[string]any{"path": "a.go", "old_string": "func A() {}", "new_string": "func A() int { return 1 }"})
	if !strings.Contains(content(), "func A() int { return 1 }") {
		t.Errorf("edit not applied: %q", content())
	}
	before := content()
	for name, args := range map[string]map[string]any{
		"not found": {"path": "a.go", "old_string": "func Z()", "new_string": "x"},
		"ambiguous": {"path": "a.go", "old_string": "func ", "new_string": "fn "},
		"no change": {"path": "a.go", "old_string": "func B", "new_string": "func B"},
		"empty old": {"path": "a.go", "old_string": "", "new_string": "x"},
		"no such":   {"path": "nope.go", "old_string": "a", "new_string": "b"},
	} {
		if res := run(t, tools["edit_file"], args); !res.IsError {
			t.Errorf("%s: want an error result", name)
		}
	}
	if content() != before {
		t.Error("a refused edit must leave the file alone")
	}
	ok(t, tools["edit_file"], map[string]any{"path": "a.go", "old_string": "func ", "new_string": "fn ", "replace_all": true})
	if strings.Contains(content(), "func ") {
		t.Errorf("replace_all left occurrences: %q", content())
	}
}

// TestGlob: results are relative to the working directory, so they can
// be passed straight to the other tools, and .git is never searched.
func TestGlob(t *testing.T) {
	_, tools := workspace(t)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"pattern": "**/*.go"}, "a.go\ndeep/b.go"},
		{map[string]any{"pattern": "*.go"}, "a.go"},
		{map[string]any{"pattern": "deep/*"}, "deep/b.go\ndeep/notes.md"},
		{map[string]any{"pattern": "**/*.md", "path": ".."}, "../README.md\ndeep/notes.md"},
		{map[string]any{"pattern": "**/config", "path": ".."}, "No files match."},
	} {
		if got := ok(t, tools["glob"], tc.args); got != tc.want {
			t.Errorf("glob(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
	if res := run(t, tools["glob"], map[string]any{"pattern": "[bad"}); !res.IsError {
		t.Error("a malformed pattern must be an error")
	}
	// A glob result reads back without adjustment.
	found := ok(t, tools["glob"], map[string]any{"pattern": "*.md", "path": ".."})
	if got := ok(t, tools["read_file"], map[string]any{"path": found}); !strings.Contains(got, "top") {
		t.Errorf("read of the glob result %q = %q", found, got)
	}
}

// TestGlob_SkipsAndCollapses: dependency trees are not searched, and
// repeated ** behave as one without collapsing ordinary segments.
func TestGlob_SkipsAndCollapses(t *testing.T) {
	root, tools := workspace(t)
	for _, name := range []string{"pkg/node_modules/dep/index.go", "pkg/deep/deep/c.go"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := ok(t, tools["glob"], map[string]any{"pattern": "**/**/**/*.go"}); got != "a.go\ndeep/b.go\ndeep/deep/c.go" {
		t.Errorf("repeated ** = %q", got)
	}
	if got := ok(t, tools["glob"], map[string]any{"pattern": "deep/deep/*.go"}); got != "deep/deep/c.go" {
		t.Errorf("deep/deep/*.go = %q, want only the nested file", got)
	}
}

// TestRead_ReplyIsCapped: a long file comes back in pieces, each
// saying where the next one starts.
func TestRead_ReplyIsCapped(t *testing.T) {
	root, tools := workspace(t)
	line := strings.Repeat("x", 1000)
	body := strings.Repeat(line+"\n", 200)
	if err := os.WriteFile(filepath.Join(root, "pkg", "long.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ok(t, tools["read_file"], map[string]any{"path": "long.txt"})
	if len(got) > maxReplyBytes+2000 || !strings.Contains(got, "more lines; continue with offset") {
		t.Fatalf("reply is %d bytes without a continuation; want it cut near %d", len(got), maxReplyBytes)
	}
	next := got[strings.LastIndex(got, "offset ")+len("offset ") : strings.LastIndex(got, "]")]
	rest := ok(t, tools["read_file"], map[string]any{"path": "long.txt", "offset": json.Number(next)})
	if !strings.HasPrefix(strings.TrimLeft(rest, " "), next+"\t") {
		t.Errorf("the continuation does not start at line %s: %.40q", next, rest)
	}
}

func TestList(t *testing.T) {
	_, tools := workspace(t)
	if got := ok(t, tools["list_dir"], map[string]any{}); got != "a.go\ndeep/" {
		t.Errorf("list = %q, want a.go and deep/", got)
	}
	if got := ok(t, tools["list_dir"], map[string]any{"path": "deep"}); got != "b.go\nnotes.md" {
		t.Errorf("list deep = %q", got)
	}
	if res := run(t, tools["list_dir"], map[string]any{"path": "../.."}); !res.IsError {
		t.Error("listing above the project must be refused")
	}
}

func TestNew_DirMustBeInsideRoot(t *testing.T) {
	if _, err := New(t.TempDir(), t.TempDir()); err == nil {
		t.Error("a working directory outside the root must be refused")
	}
}

// TestSearch: matches come back with paths the other tools accept,
// honouring case, regex and the file filter, and skipping binaries.
func TestSearch(t *testing.T) {
	root, _ := workspace(t)
	if err := os.WriteFile(filepath.Join(root, "pkg", "blob.bin"), []byte("func\x00A"), 0o644); err != nil {
		t.Fatal(err)
	}
	ft, err := New(root, filepath.Join(root, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ft.Close() }() // read-only handle; nothing to flush
	found := func(pattern string, opts search.Options) string {
		t.Helper()
		results, err := ft.Search(context.Background(), "", pattern, opts)
		if err != nil {
			t.Fatalf("Search(%q): %v", pattern, err)
		}
		var out []string
		for _, r := range results {
			out = append(out, fmt.Sprintf("%s:%d", r.Path, r.Line))
		}
		return strings.Join(out, " ")
	}
	if got := found("FUNC a", search.Options{}); got != "a.go:3" {
		t.Errorf("literal, any case = %q, want a.go:3", got)
	}
	if got := found("FUNC a", search.Options{CaseSensitive: true}); got != "" {
		t.Errorf("case-sensitive = %q, want nothing", got)
	}
	if got := found(`^func [AB]\(`, search.Options{Regex: true}); got != "a.go:3 a.go:4" {
		t.Errorf("regex = %q, want both functions", got)
	}
	if got := found("package", search.Options{FileGlob: "b.go"}); got != "deep/b.go:1" {
		t.Errorf("file filter = %q, want deep/b.go:1", got)
	}
	if got := found("package", search.Options{MaxResults: 1}); got != "a.go:1" {
		t.Errorf("limit = %q, want the first match only", got)
	}
	if _, err := ft.Search(context.Background(), "", "(", search.Options{Regex: true}); err == nil {
		t.Error("a malformed expression must be an error")
	}
}

// TestWrite_ThroughLinksAndNotOverDirectories: a link inside the
// project is written through, staying a link; a directory is refused.
func TestWrite_ThroughLinksAndNotOverDirectories(t *testing.T) {
	root, tools := workspace(t)
	pkg := filepath.Join(root, "pkg")
	if err := os.Symlink("a.go", filepath.Join(pkg, "alias.go")); err != nil {
		t.Fatal(err)
	}
	ok(t, tools["write_file"], map[string]any{"path": "alias.go", "content": "package pkg\n"})
	if data, _ := os.ReadFile(filepath.Join(pkg, "a.go")); string(data) != "package pkg\n" {
		t.Errorf("the link's target = %q, want the written content", data)
	}
	if info, err := os.Lstat(filepath.Join(pkg, "alias.go")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("alias.go must still be a link: %v, %v", info, err)
	}
	if res := run(t, tools["write_file"], map[string]any{"path": "deep", "content": "x"}); !res.IsError {
		t.Error("a write over a directory must be refused")
	}
}

// TestWrite_ReplacesInOneStep: a rewrite keeps the file's mode and
// leaves no temporary file behind.
func TestWrite_ReplacesInOneStep(t *testing.T) {
	root, tools := workspace(t)
	script := filepath.Join(root, "pkg", "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Set explicitly: the mode WriteFile asks for is cut by the umask.
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	ok(t, tools["write_file"], map[string]any{"path": "run.sh", "content": "#!/bin/sh\necho hi\n"})
	info, err := os.Stat(script)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, %v; want 0755 kept", info.Mode().Perm(), err)
	}
	if got := ok(t, tools["list_dir"], map[string]any{}); got != "a.go\ndeep/\nrun.sh" {
		t.Errorf("directory after the write = %q, want no temporary file", got)
	}
}

// TestEdit_RefusesExplosiveGrowth: the result's size is checked before
// it is built, so a small file cannot be turned into gigabytes.
func TestEdit_RefusesExplosiveGrowth(t *testing.T) {
	root, tools := workspace(t)
	file := filepath.Join(root, "pkg", "dots.txt")
	if err := os.WriteFile(file, []byte(strings.Repeat("a", 100_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	res := run(t, tools["edit_file"], map[string]any{
		"path": "dots.txt", "old_string": "a", "new_string": strings.Repeat("b", 60_000), "replace_all": true,
	})
	if !res.IsError || !strings.Contains(res.Content, "write cap") {
		t.Errorf("edit = %+v, want the write cap named", res)
	}
	if data, _ := os.ReadFile(file); len(data) != 100_000 {
		t.Errorf("the file changed: %d bytes", len(data))
	}
}

// TestMatch_BoundedWork: a pattern with many ** against a deep path
// that does not match returns at once instead of backtracking.
func TestMatch_BoundedWork(t *testing.T) {
	pattern := strings.Split(strings.Repeat("**/a/", 16)+"**/missing", "/")
	name := strings.Split(strings.Repeat("a/", 32)+"file.go", "/")
	done := make(chan bool, 1)
	go func() { done <- match(pattern, name) }()
	select {
	case got := <-done:
		if got {
			t.Error("the path must not match")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("match did not finish")
	}
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/y/a.go", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"*.go", "x/a.go", false},
		{"**", "x/y", true},
	} {
		if got := match(strings.Split(tc.pattern, "/"), strings.Split(tc.name, "/")); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestWrite_LinkThroughLinkedDirectory: "jump/.." names the parent of
// the directory jump points to, not the one jump sits in. Collapsing
// the link text first would write to an unrelated file of the same name.
func TestWrite_LinkThroughLinkedDirectory(t *testing.T) {
	root, tools := workspace(t)
	pkg := filepath.Join(root, "pkg")
	if err := os.MkdirAll(filepath.Join(pkg, "deep", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"victim.go": "decoy\n", "deep/victim.go": "real one\n"} {
		if err := os.WriteFile(filepath.Join(pkg, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, to := range map[string]string{"jump": "deep/sub", "alias.go": "jump/../victim.go"} {
		if err := os.Symlink(filepath.FromSlash(to), filepath.Join(pkg, link)); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(pkg, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ok(t, tools["edit_file"], map[string]any{"path": "alias.go", "old_string": "real one", "new_string": "edited"})
	if got := read("deep/victim.go"); got != "edited\n" {
		t.Errorf("the link's real target = %q, want the edit", got)
	}
	ok(t, tools["write_file"], map[string]any{"path": "alias.go", "content": "rewritten\n"})
	if got := read("deep/victim.go"); got != "rewritten\n" {
		t.Errorf("the link's real target = %q, want the new content", got)
	}
	if got := read("victim.go"); got != "decoy\n" {
		t.Errorf("the file of the same name beside the link changed: %q", got)
	}
	// A link whose ".." would climb out of the project is refused.
	if err := os.Symlink("../../../outside.txt", filepath.Join(pkg, "up.go")); err != nil {
		t.Fatal(err)
	}
	if res := run(t, tools["write_file"], map[string]any{"path": "up.go", "content": "x"}); !res.IsError {
		t.Error("a link that leaves the project must be refused")
	}
}
