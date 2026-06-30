package hookmap

import (
	"fmt"
	"testing"

	"github.com/latebit-io/nib/kit/hookspec"
)

func TestCCName(t *testing.T) {
	cases := []struct {
		nib    string
		wantCC string
		wantOK bool
	}{
		{"write_file", "Write", true},
		{"edit_file", "Edit", true},
		{"read_file", "Read", true},
		{"bash", "Bash", true},
		{"glob", "Glob", true},
		{"search_project", "Grep", true},
		{"list_files", "LS", true},
		// Case-insensitive on the nib name.
		{"WRITE_FILE", "Write", true},
		{"Search_Project", "Grep", true},
		// No CC counterpart.
		{"apply_patch", "", false},
		{"replace_file", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		t.Run(c.nib, func(t *testing.T) {
			cc, ok := CCName(c.nib)
			if cc != c.wantCC || ok != c.wantOK {
				t.Fatalf("CCName(%q) = (%q, %v), want (%q, %v)", c.nib, cc, ok, c.wantCC, c.wantOK)
			}
		})
	}
}

func TestNibName(t *testing.T) {
	cases := []struct {
		cc      string
		wantNib string
		wantOK  bool
	}{
		{"Write", "write_file", true},
		{"Grep", "search_project", true},
		{"LS", "list_files", true},
		// Case-insensitive on the CC name.
		{"write", "write_file", true},
		{"GREP", "search_project", true},
		{"bAsH", "bash", true},
		// CC tool nib has no builtin for.
		{"WebFetch", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		t.Run(c.cc, func(t *testing.T) {
			nib, ok := NibName(c.cc)
			if nib != c.wantNib || ok != c.wantOK {
				t.Fatalf("NibName(%q) = (%q, %v), want (%q, %v)", c.cc, nib, ok, c.wantNib, c.wantOK)
			}
		})
	}
}

// roundTrip guards the two directions against drifting apart.
func TestNameRoundTrip(t *testing.T) {
	for nib, cc := range nibToCC {
		got, ok := NibName(cc)
		if !ok || got != nib {
			t.Fatalf("NibName(CCName(%q)=%q) = (%q, %v), want %q", nib, cc, got, ok, nib)
		}
	}
}

// groupWithMatcher parses a single PreToolUse group so its matcher is
// compiled. Constructing hookspec.Group directly leaves the compiled
// regex nil (match-all), so tests must go through Parse to exercise a
// real matcher.
func groupWithMatcher(t *testing.T, matcher string) hookspec.Group {
	t.Helper()
	body := fmt.Sprintf(`{"hooks":{"PreToolUse":[{"matcher":%q,"hooks":[{"type":"command","command":"true"}]}]}}`, matcher)
	cfg, err := hookspec.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse matcher %q: %v", matcher, err)
	}
	groups := cfg.Hooks[hookspec.PreToolUse]
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	return groups[0]
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name    string
		matcher string
		nibTool string
		want    bool
	}{
		// CC-style matchers match nib tools via their alias.
		{"cc-alternation hits write", "Write|Edit|Bash", "write_file", true},
		{"cc-alternation hits bash", "Write|Edit|Bash", "bash", true},
		{"cc-alternation hits edit", "Write|Edit|Bash", "edit_file", true},
		{"cc-single hits grep alias", "Grep", "search_project", true},
		{"cc-single hits read alias", "Read", "read_file", true},
		{"cc-single misses other tool", "Write", "read_file", false},

		// nib-style matchers match the nib name directly.
		{"nib-name matcher", "write_file", "write_file", true},
		{"nib-name matcher other", "search_project", "search_project", true},

		// Empty matcher matches everything.
		{"empty matches any", "", "bash", true},
		{"empty matches unmapped", "", "apply_patch", true},

		// Unmapped tools match by nib name only.
		{"unmapped by nib name", "apply_patch", "apply_patch", true},
		{"unmapped no cc alias", "Write", "apply_patch", false},
		{"unmapped no false alias", "Edit", "replace_file", false},

		// Regex anchoring is CC's (unanchored) — substring matchers fire.
		{"unanchored substring", "ash", "bash", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := groupWithMatcher(t, c.matcher)
			if got := Matches(g, c.nibTool); got != c.want {
				t.Fatalf("Matches(matcher=%q, %q) = %v, want %v", c.matcher, c.nibTool, got, c.want)
			}
		})
	}
}
