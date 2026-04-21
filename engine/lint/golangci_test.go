package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveModuleDir(t *testing.T) {
	// Lay out a multi-module tree:
	//   root/
	//     engine/go.mod
	//     engine/lint/foo.go
	//     cmd/junto-agent/go.mod
	//     cmd/junto-agent/main.go
	root := t.TempDir()
	must := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(root, "engine", "go.mod"))
	must(filepath.Join(root, "cmd", "junto-agent", "go.mod"))
	if err := os.MkdirAll(filepath.Join(root, "engine", "lint"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		dir         string
		wantWorkDir string
		wantRelDir  string
	}{
		{"engine/lint", filepath.Join(root, "engine"), "lint"},
		{"engine", filepath.Join(root, "engine"), "."},
		{"cmd/junto-agent", filepath.Join(root, "cmd", "junto-agent"), "."},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			workDir, relDir, err := resolveModuleDir(root, tc.dir)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if workDir != tc.wantWorkDir {
				t.Errorf("workDir: got %q want %q", workDir, tc.wantWorkDir)
			}
			if relDir != tc.wantRelDir {
				t.Errorf("relDir: got %q want %q", relDir, tc.wantRelDir)
			}
		})
	}
}

func TestResolveModuleDir_noModuleFallsBack(t *testing.T) {
	// Directory without any go.mod in its ancestry: fall back to projectRoot.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir, relDir, err := resolveModuleDir(root, "src")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if workDir != root {
		t.Errorf("expected fallback to projectRoot, got workDir=%q", workDir)
	}
	if relDir != "src" {
		t.Errorf("relDir: got %q want %q", relDir, "src")
	}
}

func TestReparentFindings(t *testing.T) {
	root := "/project"
	workDir := "/project/engine"
	findings := []Finding{
		{Path: "lint/foo.go", Line: 1},
		{Path: "agent/agent.go", Line: 2},
		// Absolute path should be converted to project-relative.
		{Path: "/project/engine/lint/abs.go", Line: 3},
		// Empty path is left alone.
		{Path: "", Line: 4},
	}
	got := reparentFindings(root, workDir, findings)
	want := []string{
		"engine/lint/foo.go",
		"engine/agent/agent.go",
		"engine/lint/abs.go",
		"",
	}
	for i, f := range got {
		if f.Path != want[i] {
			t.Errorf("finding %d path: got %q want %q", i, f.Path, want[i])
		}
	}
}

func TestReparentFindings_noopWhenWorkDirEqualsRoot(t *testing.T) {
	findings := []Finding{{Path: "a/b.go"}}
	got := reparentFindings("/project", "/project", findings)
	if got[0].Path != "a/b.go" {
		t.Errorf("path should be unchanged, got %q", got[0].Path)
	}
}

func TestParseGolangciJSON_clean(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty bytes", ""},
		{"whitespace", "   \n\n  "},
		{"empty issues array", `{"Issues":[], "Report":{"Linters":[]}}`},
		{"null issues field", `{"Issues":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := parseGolangciJSON([]byte(tc.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(findings) != 0 {
				t.Errorf("expected 0 findings, got %d: %+v", len(findings), findings)
			}
		})
	}
}

func TestParseGolangciJSON_findings(t *testing.T) {
	data := `{
		"Issues": [
			{
				"FromLinter": "ineffassign",
				"Text": "ineffectual assignment to err",
				"Pos": {"Filename": "engine/agent/agent.go", "Line": 1510, "Column": 9}
			},
			{
				"FromLinter": "errcheck",
				"Text": "Error return value of fn.Close is not checked",
				"Pos": {"Filename": "engine/llm/client.go", "Line": 42, "Column": 2}
			},
			{
				"FromLinter": "",
				"Text": "linter-less finding",
				"Pos": {"Filename": "weird.go", "Line": 0, "Column": 0}
			}
		]
	}`
	findings, err := parseGolangciJSON([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(findings))
	}

	if findings[0].Path != "engine/agent/agent.go" || findings[0].Line != 1510 || findings[0].Col != 9 {
		t.Errorf("finding 0 position wrong: %+v", findings[0])
	}
	if !strings.Contains(findings[0].Message, "ineffectual assignment") {
		t.Errorf("finding 0 message missing text: %q", findings[0].Message)
	}
	if !strings.Contains(findings[0].Message, "(ineffassign)") {
		t.Errorf("finding 0 message missing linter annotation: %q", findings[0].Message)
	}

	if findings[1].Linter != "" {
		t.Errorf("parseGolangciJSON should not set Linter (stamped by Run), got %q", findings[1].Linter)
	}

	// Missing FromLinter should produce message without the parenthesized
	// annotation — not "some text ()".
	if strings.HasSuffix(findings[2].Message, "()") {
		t.Errorf("empty FromLinter should omit annotation, got %q", findings[2].Message)
	}
	if findings[2].Message != "linter-less finding" {
		t.Errorf("finding 2 message wrong: %q", findings[2].Message)
	}
}

func TestParseGolangciJSON_malformed(t *testing.T) {
	cases := []string{
		`not json at all`,
		`{"Issues":`,
		`{"Issues": "not an array"}`,
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			if _, err := parseGolangciJSON([]byte(data)); err == nil {
				t.Errorf("expected parse error for %q, got nil", data)
			}
		})
	}
}

func TestParseGolangciJSON_ignoresTrailingSummary(t *testing.T) {
	// golangci-lint v2 appends a line like "0 issues." after the JSON object.
	// The streaming decoder should stop at the end of the JSON value.
	data := `{"Issues":[{"FromLinter":"x","Text":"t","Pos":{"Filename":"a.go","Line":1,"Column":1}}]}
0 issues.`
	findings, err := parseGolangciJSON([]byte(data))
	if err != nil {
		t.Fatalf("unexpected err with trailing summary: %v", err)
	}
	if len(findings) != 1 {
		t.Errorf("expected 1 finding, got %d", len(findings))
	}
}

func TestCappedBuffer_respectsCap(t *testing.T) {
	b := cappedBuffer{cap: 10}
	n, err := b.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("short write: n=%d err=%v", n, err)
	}
	// This write exceeds the remaining 5-byte cap; only "world" fits.
	n, err = b.Write([]byte("worldexcess"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Write reports full input length so exec.Cmd does not flag a short write.
	if n != len("worldexcess") {
		t.Errorf("Write should report full input length, got %d", n)
	}
	if b.String() != "helloworld" {
		t.Errorf("buffer content: got %q want %q", b.String(), "helloworld")
	}

	// Further writes past the cap are silent no-ops.
	n, err = b.Write([]byte("zzz"))
	if err != nil || n != 3 {
		t.Fatalf("over-cap write: n=%d err=%v", n, err)
	}
	if b.String() != "helloworld" {
		t.Errorf("over-cap write must not append: got %q", b.String())
	}
}

func TestCappedBuffer_uncappedIsUnlimited(t *testing.T) {
	b := cappedBuffer{} // cap == 0 → no limit
	big := strings.Repeat("x", 1<<16)
	if _, err := b.Write([]byte(big)); err != nil {
		t.Fatalf("Write err: %v", err)
	}
	if b.buf.Len() != len(big) {
		t.Errorf("uncapped buffer length: got %d want %d", b.buf.Len(), len(big))
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"hello", "hello"},
		{"  hello  ", "hello"},
		{"\n\n\nhello\nworld", "hello"},
		{"\n\n   \n", ""},
	}
	for _, tc := range cases {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
