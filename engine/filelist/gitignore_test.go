package filelist

import (
	"testing"

	"github.com/latebit-io/junto/engine/glob"
)

func TestParseLine(t *testing.T) {
	tests := []struct {
		line     string
		wantOK   bool
		glob     string
		negated  bool
		dirOnly  bool
		anchored bool
	}{
		{"", false, "", false, false, false},
		{"# comment", false, "", false, false, false},
		{"*.log", true, "*.log", false, false, false},
		{"!important.log", true, "important.log", true, false, false},
		{"build/", true, "build", false, true, false},
		{"/root.txt", true, "root.txt", false, false, true},
		{"path/to/file", true, "path/to/file", false, false, true},
		{"**/*.tmp", true, "**/*.tmp", false, false, false},
		{"\\#not-a-comment", true, "#not-a-comment", false, false, false},
		{"  ", false, "", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			p, ok := parseLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if p.glob != tt.glob {
				t.Errorf("glob = %q, want %q", p.glob, tt.glob)
			}
			if p.negated != tt.negated {
				t.Errorf("negated = %v, want %v", p.negated, tt.negated)
			}
			if p.dirOnly != tt.dirOnly {
				t.Errorf("dirOnly = %v, want %v", p.dirOnly, tt.dirOnly)
			}
			if p.anchored != tt.anchored {
				t.Errorf("anchored = %v, want %v", p.anchored, tt.anchored)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// Exact
		{"foo.txt", "foo.txt", true},
		{"foo.txt", "bar.txt", false},

		// Star glob
		{"*.log", "error.log", true},
		{"*.log", "error.txt", false},
		{"*.log", "dir/error.log", false}, // * doesn't cross /

		// Double star
		{"**/*.log", "error.log", true},
		{"**/*.log", "dir/error.log", true},
		{"**/*.log", "a/b/c/error.log", true},
		{"**/*.log", "error.txt", false},
		{"**/foo", "foo", true},
		{"**/foo", "dir/foo", true},
		{"**/foo", "a/b/foo", true},
		{"**/foo", "barfoo", false},     // ** must match at segment boundaries
		{"**/foo", "dir/barfoo", false}, // not mid-segment

		// Question mark
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},

		// Directory patterns
		{"build", "build", true},
		{"build", "rebuild", false},

		// Path patterns
		{"src/main.go", "src/main.go", true},
		{"src/main.go", "other/main.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.name, func(t *testing.T) {
			got := glob.Match(tt.pattern, tt.name)
			if got != tt.want {
				t.Errorf("glob.Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}

func TestMatcherMatch(t *testing.T) {
	m := &matcher{
		patterns: []pattern{
			{glob: "*.log", negated: false, dirOnly: false, anchored: false},
			{glob: "important.log", negated: true, dirOnly: false, anchored: false},
		},
	}

	tests := []struct {
		path    string
		isDir   bool
		wantM   bool
		wantNeg bool
	}{
		{"error.log", false, true, false},    // matches *.log only
		{"important.log", false, true, true}, // matches *.log then !important.log (negated)
		{"readme.md", false, false, false},   // no match
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			matched, neg := m.match(tt.path, tt.isDir)
			if matched != tt.wantM {
				t.Errorf("matched = %v, want %v", matched, tt.wantM)
			}
			if neg != tt.wantNeg {
				t.Errorf("negated = %v, want %v", neg, tt.wantNeg)
			}
		})
	}
}

func TestMatcherDirOnly(t *testing.T) {
	m := &matcher{
		patterns: []pattern{
			{glob: "build", negated: false, dirOnly: true, anchored: false},
		},
	}

	// Should match directory.
	matched, _ := m.match("build", true)
	if !matched {
		t.Error("dirOnly pattern should match directory")
	}

	// Should not match file.
	matched, _ = m.match("build", false)
	if matched {
		t.Error("dirOnly pattern should not match file")
	}
}

func TestMatcherNil(t *testing.T) {
	var m *matcher
	matched, neg := m.match("anything", false)
	if matched || neg {
		t.Error("nil matcher should not match")
	}
}
