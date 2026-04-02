package editor

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/latebit-io/junto/engine/buffer"
)

//nolint:funlen // table-driven test — length comes from test cases, not complexity
func TestNarrowEdit(t *testing.T) {
	tests := []struct {
		name        string
		search      string
		replace     string
		editLine    int
		editCol     int
		wantLine    int
		wantCol     int
		wantSearch  string
		wantReplace string
		wantPrefix  int
		wantSuffix  int
	}{
		{
			name:        "middle line changed",
			search:      "aaa\nbbb\nccc",
			replace:     "aaa\nXXX\nccc",
			editLine:    0,
			editCol:     0,
			wantLine:    1,
			wantCol:     0,
			wantSearch:  "bbb",
			wantReplace: "XXX",
			wantPrefix:  1,
			wantSuffix:  1,
		},
		{
			name:        "all lines changed",
			search:      "aaa\nbbb",
			replace:     "XXX\nYYY",
			editLine:    5,
			editCol:     3,
			wantLine:    5,
			wantCol:     3,
			wantSearch:  "aaa\nbbb",
			wantReplace: "XXX\nYYY",
			wantPrefix:  0,
			wantSuffix:  0,
		},
		{
			name:        "prefix only unchanged",
			search:      "aaa\nbbb\nccc",
			replace:     "aaa\nXXX\nYYY",
			editLine:    0,
			editCol:     0,
			wantLine:    1,
			wantCol:     0,
			wantSearch:  "bbb\nccc",
			wantReplace: "XXX\nYYY",
			wantPrefix:  1,
			wantSuffix:  0,
		},
		{
			name:        "suffix only unchanged",
			search:      "aaa\nbbb\nccc",
			replace:     "XXX\nYYY\nccc",
			editLine:    0,
			editCol:     0,
			wantLine:    0,
			wantCol:     0,
			wantSearch:  "aaa\nbbb",
			wantReplace: "XXX\nYYY",
			wantPrefix:  0,
			wantSuffix:  1,
		},
		{
			name:        "all lines identical",
			search:      "aaa\nbbb",
			replace:     "aaa\nbbb",
			editLine:    3,
			editCol:     0,
			wantLine:    4,
			wantCol:     0,
			wantSearch:  "bbb",
			wantReplace: "bbb",
			wantPrefix:  1,
			wantSuffix:  0,
		},
		{
			name:        "single line changed",
			search:      "aaa",
			replace:     "XXX",
			editLine:    0,
			editCol:     5,
			wantLine:    0,
			wantCol:     5,
			wantSearch:  "aaa",
			wantReplace: "XXX",
			wantPrefix:  0,
			wantSuffix:  0,
		},
		{
			name:        "insert in middle",
			search:      "aaa\nccc",
			replace:     "aaa\nbbb\nccc",
			editLine:    0,
			editCol:     0,
			wantLine:    0,
			wantCol:     0,
			wantSearch:  "aaa",
			wantReplace: "aaa\nbbb",
			wantPrefix:  0,
			wantSuffix:  1,
		},
		{
			name:        "delete in middle",
			search:      "aaa\nbbb\nccc\nddd",
			replace:     "aaa\nddd",
			editLine:    0,
			editCol:     0,
			wantLine:    1,
			wantCol:     0,
			wantSearch:  "bbb\nccc",
			wantReplace: "",
			wantPrefix:  1,
			wantSuffix:  1,
		},
		{
			name:        "mid-line edit col preserved when no prefix",
			search:      "hello world",
			replace:     "hello earth",
			editLine:    7,
			editCol:     10,
			wantLine:    7,
			wantCol:     10,
			wantSearch:  "hello world",
			wantReplace: "hello earth",
			wantPrefix:  0,
			wantSuffix:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searchLines := splitLines(tt.search)
			replaceLines := splitLines(tt.replace)
			hunks := ComputeHunks(searchLines, replaceLines)

			ne := NarrowEdit(tt.editLine, tt.editCol, tt.search, tt.replace, hunks, nil)

			if ne.Line != tt.wantLine {
				t.Errorf("Line = %d, want %d", ne.Line, tt.wantLine)
			}
			if ne.Col != tt.wantCol {
				t.Errorf("Col = %d, want %d", ne.Col, tt.wantCol)
			}
			if ne.Search != tt.wantSearch {
				t.Errorf("Search = %q, want %q", ne.Search, tt.wantSearch)
			}
			if ne.Replace != tt.wantReplace {
				t.Errorf("Replace = %q, want %q", ne.Replace, tt.wantReplace)
			}
			if ne.PrefixLines != tt.wantPrefix {
				t.Errorf("PrefixLines = %d, want %d", ne.PrefixLines, tt.wantPrefix)
			}
			if ne.SuffixLines != tt.wantSuffix {
				t.Errorf("SuffixLines = %d, want %d", ne.SuffixLines, tt.wantSuffix)
			}
		})
	}
}

// splitLines splits text by newline, matching strings.Split used in production.
func splitLines(text string) []string {
	return strings.Split(text, "\n")
}

func TestNarrowEditOrigins(t *testing.T) {
	agent := buffer.OriginAgent
	dev := buffer.OriginDeveloper
	origins := []*buffer.Origin{nil, &agent, &dev, nil}
	// search: 4 lines (aaa, bbb, ccc, ddd), prefix=1 (aaa), suffix=1 (ddd)
	// narrowed origins should be [&agent, &dev] (indices 1-2)
	hunks := ComputeHunks(
		[]string{"aaa", "bbb", "ccc", "ddd"},
		[]string{"aaa", "XXX", "YYY", "ddd"},
	)
	ne := NarrowEdit(0, 0, "aaa\nbbb\nccc\nddd", "aaa\nXXX\nYYY\nddd", hunks, origins)

	if len(ne.LineOrigins) != 2 {
		t.Fatalf("len(LineOrigins) = %d, want 2", len(ne.LineOrigins))
	}
	if ne.LineOrigins[0] == nil || *ne.LineOrigins[0] != agent {
		t.Errorf("LineOrigins[0] = %v, want OriginAgent", ne.LineOrigins[0])
	}
	if ne.LineOrigins[1] == nil || *ne.LineOrigins[1] != dev {
		t.Errorf("LineOrigins[1] = %v, want OriginDeveloper", ne.LineOrigins[1])
	}
}

// TestNarrowEditIntegration verifies the full flow: narrow + IncrementalEdit.
func TestNarrowEditIntegration(t *testing.T) {
	buf := buffer.New()
	buf.Insert(0, 0, "aaa\nbbb\nccc\nddd")
	e := New(buf)

	search := "aaa\nbbb\nccc\nddd"
	replace := "aaa\nXXX\nYYY\nddd"
	hunks := ComputeHunks(
		splitLines(search),
		splitLines(replace),
	)
	ne := NarrowEdit(0, 0, search, replace, hunks, nil)

	// Create IncrementalEdit with narrowed values.
	searchRunes := utf8.RuneCountInString(ne.Search)
	ie := e.BeginIncrementalEdit(ne.Line, ne.Col, searchRunes, 100, ne.Replace, ne.LineOrigins)

	// Drain the edit.
	for range 1000 {
		r := ie.Advance()
		if r.Done {
			break
		}
	}
	ie.Complete()

	got := buf.Content()
	want := "aaa\nXXX\nYYY\nddd"
	if got != want {
		t.Errorf("buffer = %q, want %q", got, want)
	}

	// Undo should revert atomically.
	_, _, ok := buf.Undo()
	if !ok {
		t.Fatal("undo failed")
	}
	got = buf.Content()
	if got != "aaa\nbbb\nccc\nddd" {
		t.Errorf("after undo: %q, want original", got)
	}
}

// TestNarrowEditIntegrationInsert verifies pure-insertion surgical edits.
// When the narrowed search is empty (all search lines are in prefix/suffix),
// the replacement must not concatenate with existing line content.
func TestNarrowEditIntegrationInsert(t *testing.T) {
	buf := buffer.New()
	buf.Insert(0, 0, "aaa\nccc")
	e := New(buf)

	search := "aaa\nccc"
	replace := "aaa\nbbb\nccc"
	hunks := ComputeHunks(splitLines(search), splitLines(replace))
	ne := NarrowEdit(0, 0, search, replace, hunks, nil)

	searchRunes := utf8.RuneCountInString(ne.Search)
	ie := e.BeginIncrementalEdit(ne.Line, ne.Col, searchRunes, 100, ne.Replace, ne.LineOrigins)

	for range 1000 {
		r := ie.Advance()
		if r.Done {
			break
		}
	}
	ie.Complete()

	got := buf.Content()
	want := "aaa\nbbb\nccc"
	if got != want {
		t.Errorf("buffer = %q, want %q", got, want)
	}
}

// TestNarrowEditIntegrationInsertMultiLine verifies multi-line pure insertion.
func TestNarrowEditIntegrationInsertMultiLine(t *testing.T) {
	buf := buffer.New()
	buf.Insert(0, 0, "func main() {\n\tfmt.Println(\"done\")\n}")
	e := New(buf)

	search := "func main() {\n\tfmt.Println(\"done\")\n}"
	replace := "func main() {\n\tx := 1\n\ty := 2\n\tfmt.Println(\"done\")\n}"
	hunks := ComputeHunks(splitLines(search), splitLines(replace))
	ne := NarrowEdit(0, 0, search, replace, hunks, nil)

	searchRunes := utf8.RuneCountInString(ne.Search)
	ie := e.BeginIncrementalEdit(ne.Line, ne.Col, searchRunes, 100, ne.Replace, ne.LineOrigins)

	for range 1000 {
		r := ie.Advance()
		if r.Done {
			break
		}
	}
	ie.Complete()

	got := buf.Content()
	if got != replace {
		t.Errorf("buffer = %q, want %q", got, replace)
	}
}

func TestIsSurgical(t *testing.T) {
	tests := []struct {
		name string
		s, r []string
		want bool
	}{
		{"all different", []string{"a", "b"}, []string{"x", "y"}, false},
		{"has keep", []string{"a", "b", "c"}, []string{"a", "X", "c"}, true},
		{"all same", []string{"a"}, []string{"a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hunks := ComputeHunks(tt.s, tt.r)
			got := IsSurgical(hunks)
			if got != tt.want {
				t.Errorf("IsSurgical = %v, want %v", got, tt.want)
			}
		})
	}
}
