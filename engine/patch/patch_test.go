package patch

import (
	"errors"
	"strings"
	"testing"
)

func TestParse_ValidSingleHunk(t *testing.T) {
	raw := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: hello.go",
		"@@ func main",
		" fmt.Println(\"hello\")",
		"-x := 1",
		"+x := 2",
		"*** End Patch",
	}, "\n")

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("Files: want 1, got %d", len(got.Files))
	}
	f := got.Files[0]
	if f.Path != "hello.go" {
		t.Errorf("Path: want hello.go, got %q", f.Path)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("Hunks: want 1, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.Anchor != "func main" {
		t.Errorf("Anchor: want %q, got %q", "func main", h.Anchor)
	}
	wantLines := []HunkLine{
		{Kind: HunkContext, Text: "fmt.Println(\"hello\")"},
		{Kind: HunkDelete, Text: "x := 1"},
		{Kind: HunkInsert, Text: "x := 2"},
	}
	if len(h.Lines) != len(wantLines) {
		t.Fatalf("Lines: want %d, got %d", len(wantLines), len(h.Lines))
	}
	for i, want := range wantLines {
		if h.Lines[i] != want {
			t.Errorf("Lines[%d]: want %+v, got %+v", i, want, h.Lines[i])
		}
	}
}

func TestParse_ValidMultipleHunks(t *testing.T) {
	raw := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.go",
		"@@ funcA",
		"-old A",
		"+new A",
		"@@ funcB",
		"-old B",
		"+new B",
		"*** End Patch",
	}, "\n")

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Files) != 1 || len(got.Files[0].Hunks) != 2 {
		t.Fatalf("want 1 file, 2 hunks; got %d files, %d hunks",
			len(got.Files), len(got.Files[0].Hunks))
	}
	if got.Files[0].Hunks[0].Anchor != "funcA" || got.Files[0].Hunks[1].Anchor != "funcB" {
		t.Errorf("anchors: got %q / %q",
			got.Files[0].Hunks[0].Anchor, got.Files[0].Hunks[1].Anchor)
	}
}

func TestParse_NoAnchorImplicitHunk(t *testing.T) {
	raw := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.go",
		"-old",
		"+new",
		"*** End Patch",
	}, "\n")

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Files[0].Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(got.Files[0].Hunks))
	}
	if got.Files[0].Hunks[0].Anchor != "" {
		t.Errorf("Anchor: want empty, got %q", got.Files[0].Hunks[0].Anchor)
	}
}

func TestParse_CRLFLineEndings(t *testing.T) {
	raw := "*** Begin Patch\r\n*** Update File: a.go\r\n-x\r\n+y\r\n*** End Patch\r\n"
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Files[0].Hunks[0].Lines[0] != (HunkLine{Kind: HunkDelete, Text: "x"}) {
		t.Errorf("delete: %+v", got.Files[0].Hunks[0].Lines[0])
	}
}

func TestParse_TrailingNewlineTolerated(t *testing.T) {
	raw := "*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** End Patch\n"
	if _, err := Parse(raw); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParse_LeadingBlankLinesTolerated(t *testing.T) {
	raw := "\n\n*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** End Patch"
	if _, err := Parse(raw); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParse_BlankContextLine(t *testing.T) {
	// Empty hunk line (no prefix at all) treated as empty context.
	raw := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.go",
		"@@",
		" fmt.Println(\"hello\")",
		"",
		"-x := 1",
		"+x := 2",
		"*** End Patch",
	}, "\n")
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lines := got.Files[0].Hunks[0].Lines
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d: %+v", len(lines), lines)
	}
	if lines[1] != (HunkLine{Kind: HunkContext, Text: ""}) {
		t.Errorf("blank context: got %+v", lines[1])
	}
}

// Table-driven coverage of every error path. Each case names the
// sentinel the caller should be able to detect via errors.Is.
//
//nolint:funlen // table-driven; length comes from cases, not complexity
func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want error
	}{
		{
			name: "missing begin — empty input",
			raw:  "",
			want: ErrMissingBegin,
		},
		{
			name: "missing begin — wrong first line",
			raw:  "*** Update File: a.go\n-x\n+y\n*** End Patch",
			want: ErrMissingBegin,
		},
		{
			name: "missing end",
			raw:  "*** Begin Patch\n*** Update File: a.go\n-x\n+y",
			want: ErrMissingEnd,
		},
		{
			name: "unknown directive",
			raw:  "*** Begin Patch\n*** Update File: a.go\n*** Move File: a.go b.go\n-x\n+y\n*** End Patch",
			want: ErrUnknownDirective,
		},
		{
			name: "add file not supported",
			raw:  "*** Begin Patch\n*** Add File: new.go\n+package new\n*** End Patch",
			want: &UnsupportedV2Error{Directive: "Add File"},
		},
		{
			name: "delete file not supported",
			raw:  "*** Begin Patch\n*** Delete File: old.go\n*** End Patch",
			want: &UnsupportedV2Error{Directive: "Delete File"},
		},
		{
			name: "multi-file rejected",
			raw:  "*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** Update File: b.go\n-x\n+y\n*** End Patch",
			want: ErrMultiFile,
		},
		{
			name: "hunk before file",
			raw:  "*** Begin Patch\n-x\n+y\n*** End Patch",
			want: ErrNoFile,
		},
		{
			name: "hunk with no -/+ lines",
			raw:  "*** Begin Patch\n*** Update File: a.go\n@@ funcA\n context only\n more context\n*** End Patch",
			want: ErrEmptyHunk,
		},
		{
			name: "file with no hunks",
			raw:  "*** Begin Patch\n*** Update File: a.go\n*** End Patch",
			want: ErrEmptyHunk,
		},
		{
			name: "missing path",
			raw:  "*** Begin Patch\n*** Update File:\n-x\n+y\n*** End Patch",
			want: ErrMissingPath,
		},
		{
			name: "trailing content after end",
			raw:  "*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** End Patch\nstray text",
			want: ErrUnknownDirective,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw)
			if err == nil {
				t.Fatalf("want error %v, got nil", tt.want)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("want errors.Is(%v); got %v", tt.want, err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("want *ParseError wrapper; got %T: %v", err, err)
			}
		})
	}
}

func TestParse_ErrorLineNumbers(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantLine int
	}{
		{
			name:     "wrong first line points at line 1",
			raw:      "not a patch\n*** Update File: a.go",
			wantLine: 1,
		},
		{
			name:     "unknown directive points at the directive line",
			raw:      "*** Begin Patch\n*** Update File: a.go\n*** Move File\n*** End Patch",
			wantLine: 3,
		},
		{
			name:     "empty hunk points at the @@ line",
			raw:      "*** Begin Patch\n*** Update File: a.go\n@@ funcA\n only context\n*** End Patch",
			wantLine: 3,
		},
		{
			name:     "trailing content points at the stray line",
			raw:      "*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** End Patch\nstray",
			wantLine: 6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.raw)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("want *ParseError, got %T: %v", err, err)
			}
			if pe.Line != tt.wantLine {
				t.Errorf("Line: want %d, got %d (err: %v)", tt.wantLine, pe.Line, pe)
			}
		})
	}
}

func TestParse_UnsupportedV2ErrorCarriesDirective(t *testing.T) {
	_, err := Parse("*** Begin Patch\n*** Add File: new.go\n+package new\n*** End Patch")
	var v2 *UnsupportedV2Error
	if !errors.As(err, &v2) {
		t.Fatalf("want *UnsupportedV2Error, got %T: %v", err, err)
	}
	if v2.Directive != "Add File" {
		t.Errorf("Directive: want %q, got %q", "Add File", v2.Directive)
	}
}

func TestClassifyHunkLine(t *testing.T) {
	tests := []struct {
		in       string
		wantKind HunkKind
		wantText string
	}{
		{"-foo", HunkDelete, "foo"},
		{"+foo", HunkInsert, "foo"},
		{" foo", HunkContext, "foo"},
		{"", HunkContext, ""},
		{"bare", HunkContext, "bare"}, // lenient context for missing-prefix lines
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			kind, text := classifyHunkLine(tt.in)
			if kind != tt.wantKind || text != tt.wantText {
				t.Errorf("classifyHunkLine(%q): got (%d, %q), want (%d, %q)",
					tt.in, kind, text, tt.wantKind, tt.wantText)
			}
		})
	}
}
