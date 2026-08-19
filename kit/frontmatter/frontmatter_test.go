package frontmatter

import (
	"strings"
	"testing"
	"time"
)

type meta struct {
	Name string `yaml:"name"`
	Desc string `yaml:"description"`
}

func TestSplit(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantBody string
		wantMeta meta
		wantErr  bool
	}{
		{name: "no frontmatter", in: "hello\n---\nworld\n", wantBody: "hello\n---\nworld\n"},
		{name: "empty input", in: "", wantBody: ""},
		{
			name:     "normal",
			in:       "---\nname: a\ndescription: b\n---\nbody\n",
			wantBody: "body\n",
			wantMeta: meta{Name: "a", Desc: "b"},
		},
		{
			name:     "CRLF",
			in:       "---\r\nname: a\r\n---\r\nbody\r\n",
			wantBody: "body\r\n",
			wantMeta: meta{Name: "a"},
		},
		{
			name:     "empty body",
			in:       "---\nname: a\n---",
			wantBody: "",
			wantMeta: meta{Name: "a"},
		},
		{
			name:     "empty frontmatter",
			in:       "---\n---\nbody",
			wantBody: "body",
		},
		{
			name:     "BOM stripped",
			in:       "\xef\xbb\xbf---\nname: a\n---\nbody",
			wantBody: "body",
			wantMeta: meta{Name: "a"},
		},
		{
			name:     "unterminated fence is all body",
			in:       "---\nname: a\nbody",
			wantBody: "---\nname: a\nbody",
		},
		{
			name:     "fence with trailing content is body",
			in:       "--- x\nname: a\n---\nbody",
			wantBody: "--- x\nname: a\n---\nbody",
		},
		{
			name:    "malformed yaml",
			in:      "---\nname: [\n---\nbody",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m meta
			body, err := Split([]byte(tc.in), &m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if m != tc.wantMeta {
				t.Errorf("meta = %+v, want %+v", m, tc.wantMeta)
			}
		})
	}
}

// TestSplitHugeUnterminated guards the O(n) scan: 1 MiB of short lines
// with no closing fence must complete quickly rather than rebuilding
// the accumulated frontmatter on every line.
func TestSplitHugeUnterminated(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("---\n")
	for sb.Len() < 1<<20 {
		sb.WriteString("k: v\n")
	}
	in := sb.String()
	start := time.Now()
	var m meta
	body, err := Split([]byte(in), &m)
	if err != nil {
		t.Fatal(err)
	}
	if body != in {
		t.Fatal("unterminated fence must return whole document as body")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Split took %v on 1 MiB unterminated input", d)
	}
}
