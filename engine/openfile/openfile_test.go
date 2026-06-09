package openfile

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
			o := New(buf)

			got := o.ComputeDiff(tt.search, tt.replace)

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
				t.Fatalf("NewLines len = %d, want %d\ngot:  %q\nwant: %q",
					len(got.NewLines), len(tt.newLines), got.NewLines, tt.newLines)
			}
			for i, line := range got.NewLines {
				if line != tt.newLines[i] {
					t.Errorf("NewLines[%d] = %q, want %q", i, line, tt.newLines[i])
				}
			}
		})
	}
}

func TestLocateEdit(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		search      string
		wantLoc     EditLocation
		wantReason  string
		wantSuccess bool
	}{
		{
			name:        "single match first line",
			content:     "hello world",
			search:      "world",
			wantLoc:     EditLocation{Line: 0, Col: 6},
			wantSuccess: true,
		},
		{
			name:        "single match later line",
			content:     "a\nb\nfoo bar\nc",
			search:      "bar",
			wantLoc:     EditLocation{Line: 2, Col: 4},
			wantSuccess: true,
		},
		{
			name:       "no match",
			content:    "hello",
			search:     "xyz",
			wantReason: "Edit could not be applied — text not found",
		},
		{
			name:       "multiple matches",
			content:    "aa",
			search:     "a",
			wantReason: "Edit could not be applied — 2 matches found, expected 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := buffer.New()
			buf.Insert(0, 0, tt.content)
			o := New(buf)

			loc, reason := o.LocateEdit(tt.search)

			if tt.wantSuccess {
				if reason != "" {
					t.Fatalf("expected success, got reason %q", reason)
				}
				if loc != tt.wantLoc {
					t.Errorf("location = %+v, want %+v", loc, tt.wantLoc)
				}
				return
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestApplyEdit(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		search        string
		replace       string
		wantApplied   bool
		wantContent   string
		wantNewCursor EditLocation
		wantReason    string
	}{
		{
			name:          "single line replace",
			content:       "hello world",
			search:        "world",
			replace:       "there",
			wantApplied:   true,
			wantContent:   "hello there",
			wantNewCursor: EditLocation{Line: 0, Col: 6},
		},
		{
			name:          "multi-line replace",
			content:       "a\nb\nc",
			search:        "b",
			replace:       "B1\nB2",
			wantApplied:   true,
			wantContent:   "a\nB1\nB2\nc",
			wantNewCursor: EditLocation{Line: 1, Col: 0},
		},
		{
			name:        "not found",
			content:     "hello",
			search:      "world",
			replace:     "x",
			wantApplied: false,
			wantReason:  "Edit could not be applied — text not found",
		},
		{
			name:        "multiple matches",
			content:     "aa",
			search:      "a",
			replace:     "b",
			wantApplied: false,
			wantReason:  "Edit could not be applied — 2 matches found, expected 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := buffer.New()
			buf.Insert(0, 0, tt.content)
			o := New(buf)

			out := o.ApplyEdit(tt.search, tt.replace, nil)

			if out.Applied != tt.wantApplied {
				t.Fatalf("Applied = %v, want %v (reason=%q)", out.Applied, tt.wantApplied, out.FailureReason)
			}
			if !tt.wantApplied {
				if out.FailureReason != tt.wantReason {
					t.Errorf("FailureReason = %q, want %q", out.FailureReason, tt.wantReason)
				}
				return
			}
			if got := o.Content(); got != tt.wantContent {
				t.Errorf("content = %q, want %q", got, tt.wantContent)
			}
			if out.NewCursor != tt.wantNewCursor {
				t.Errorf("NewCursor = %+v, want %+v", out.NewCursor, tt.wantNewCursor)
			}
		})
	}
}

func TestApplyEditLineOrigins(t *testing.T) {
	buf := buffer.New()
	buf.Insert(0, 0, "alpha\nbeta\ngamma")
	o := New(buf)

	agentOrigin := OriginAgent
	out := o.ApplyEdit("beta", "BETA1\nBETA2", []*LineOrigin{&agentOrigin, &agentOrigin})

	if !out.Applied {
		t.Fatalf("expected Applied=true, got reason %q", out.FailureReason)
	}
	if got := o.Content(); got != "alpha\nBETA1\nBETA2\ngamma" {
		t.Errorf("content = %q", got)
	}
	if got := o.Buf.LineOrigin(1); got != OriginAgent {
		t.Errorf("line 1 origin = %v, want OriginAgent", got)
	}
	if got := o.Buf.LineOrigin(2); got != OriginAgent {
		t.Errorf("line 2 origin = %v, want OriginAgent", got)
	}
}

func TestReplaceRange(t *testing.T) {
	buf := buffer.New()
	buf.Insert(0, 0, "hello world")
	o := New(buf)

	o.ReplaceRange(0, 6, 5, "there")

	if got := o.Content(); got != "hello there" {
		t.Errorf("content = %q, want %q", got, "hello there")
	}
}

func TestSaveModifiedPath(t *testing.T) {
	buf := buffer.New()
	o := New(buf)

	if got := o.Path(); got != "" {
		t.Errorf("Path() on fresh buffer = %q, want empty", got)
	}
	if o.Modified() {
		t.Error("Modified() true on fresh buffer")
	}
	if got := o.Content(); got != "" {
		t.Errorf("Content() on fresh buffer = %q, want empty", got)
	}
}
