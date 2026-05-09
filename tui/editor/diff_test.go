package editor

import (
	"testing"

	"github.com/latebit-io/nib/engine/buffer"
)

//nolint:funlen // table-driven test — length comes from test cases, not complexity
func TestComputeDiff(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		search    string
		replace   string
		wantNil   bool
		startLine int
		endLine   int
		newLines  []string
	}{
		{
			name:      "single line replacement",
			content:   "hello world",
			search:    "world",
			replace:   "earth",
			startLine: 0,
			endLine:   0,
			newLines:  []string{"hello earth"},
		},
		{
			name:      "multi-line file single line match",
			content:   "line one\nline two\nline three",
			search:    "line two",
			replace:   "line TWO",
			startLine: 1,
			endLine:   1,
			newLines:  []string{"line TWO"},
		},
		{
			name:      "match spans multiple lines",
			content:   "func main() {\n\tfmt.Println(\"hello\")\n}",
			search:    "func main() {\n\tfmt.Println(\"hello\")\n}",
			replace:   "func main() {\n\tfmt.Println(\"goodbye\")\n\tfmt.Println(\"world\")\n}",
			startLine: 0,
			endLine:   2,
			newLines:  []string{"func main() {", "\tfmt.Println(\"goodbye\")", "\tfmt.Println(\"world\")", "}"},
		},
		{
			name:      "replacement adds lines",
			content:   "a\nb\nc",
			search:    "b",
			replace:   "b1\nb2\nb3",
			startLine: 1,
			endLine:   1,
			newLines:  []string{"b1", "b2", "b3"},
		},
		{
			name:      "replacement removes lines",
			content:   "a\nb\nc\nd\ne",
			search:    "b\nc\nd",
			replace:   "X",
			startLine: 1,
			endLine:   3,
			newLines:  []string{"X"},
		},
		{
			name:      "partial line match preserves prefix and suffix",
			content:   "  foo bar baz  ",
			search:    "bar",
			replace:   "BAR",
			startLine: 0,
			endLine:   0,
			newLines:  []string{"  foo BAR baz  "},
		},
		{
			name:      "match at start of file",
			content:   "hello\nworld",
			search:    "hello",
			replace:   "HELLO",
			startLine: 0,
			endLine:   0,
			newLines:  []string{"HELLO"},
		},
		{
			name:      "match at end of file",
			content:   "hello\nworld",
			search:    "world",
			replace:   "WORLD",
			startLine: 1,
			endLine:   1,
			newLines:  []string{"WORLD"},
		},
		{
			name:    "no match returns nil",
			content: "hello world",
			search:  "xyz",
			replace: "abc",
			wantNil: true,
		},
		{
			name:    "multiple matches returns nil",
			content: "aaa",
			search:  "a",
			replace: "b",
			wantNil: true,
		},
		{
			name:      "multi-line match mid-line start and end",
			content:   "prefix START\nmiddle\nEND suffix",
			search:    "START\nmiddle\nEND",
			replace:   "REPLACED",
			startLine: 0,
			endLine:   2,
			newLines:  []string{"prefix REPLACED suffix"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := buffer.New()
			buf.Insert(0, 0, tt.content)
			e := New(buf)

			got := e.ComputeDiff(tt.search, tt.replace)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil DiffResult, got nil")
			}
			if got.StartLine != tt.startLine {
				t.Errorf("StartLine = %d, want %d", got.StartLine, tt.startLine)
			}
			if got.EndLine != tt.endLine {
				t.Errorf("EndLine = %d, want %d", got.EndLine, tt.endLine)
			}
			if len(got.NewLines) != len(tt.newLines) {
				t.Fatalf("NewLines len = %d, want %d\ngot:  %q\nwant: %q", len(got.NewLines), len(tt.newLines), got.NewLines, tt.newLines)
			}
			for i, line := range got.NewLines {
				if line != tt.newLines[i] {
					t.Errorf("NewLines[%d] = %q, want %q", i, line, tt.newLines[i])
				}
			}
		})
	}
}
