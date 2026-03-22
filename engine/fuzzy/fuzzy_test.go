package fuzzy

import (
	"testing"
)

func TestScore_ExactMatch(t *testing.T) {
	m := Score("session.go", "session.go")
	if m.Score == 0 {
		t.Fatal("exact match should have score > 0")
	}

	// Case-insensitive exact.
	m2 := Score("Session.Go", "session.go")
	if m2.Score == 0 {
		t.Fatal("case-insensitive exact match should have score > 0")
	}
	if m2.Score != m.Score {
		t.Errorf("case-insensitive exact should score the same: got %d, want %d", m2.Score, m.Score)
	}
}

func TestScore_PrefixMatch(t *testing.T) {
	m := Score("ses", "session.go")
	if m.Score == 0 {
		t.Fatal("prefix match should have score > 0")
	}

	// Prefix should score lower than exact.
	exact := Score("session.go", "session.go")
	if m.Score >= exact.Score {
		t.Errorf("prefix (%d) should score lower than exact (%d)", m.Score, exact.Score)
	}
}

func TestScore_SubstringMatch(t *testing.T) {
	m := Score("ssion", "session.go")
	if m.Score == 0 {
		t.Fatal("substring match should have score > 0")
	}

	// Substring should score lower than prefix.
	prefix := Score("ses", "session.go")
	if m.Score >= prefix.Score {
		t.Errorf("substring (%d) should score lower than prefix (%d)", m.Score, prefix.Score)
	}
}

func TestScore_SubstringAfterSeparator(t *testing.T) {
	// "session" appears right after "/" — should get separator bonus.
	m := Score("session", "engine/session/session.go")
	if m.Score == 0 {
		t.Fatal("substring after separator should match")
	}
}

func TestScore_FuzzyMatch(t *testing.T) {
	m := Score("sgo", "session.go")
	if m.Score == 0 {
		t.Fatal("fuzzy match should have score > 0")
	}

	// Fuzzy should score lower than substring.
	substr := Score("ssion", "session.go")
	if m.Score >= substr.Score {
		t.Errorf("fuzzy (%d) should score lower than substring (%d)", m.Score, substr.Score)
	}
}

func TestScore_NoMatch(t *testing.T) {
	m := Score("xyz", "session.go")
	if m.Score != 0 {
		t.Errorf("non-matching query should score 0, got %d", m.Score)
	}
}

func TestScore_EmptyQuery(t *testing.T) {
	m := Score("", "anything")
	if m.Score == 0 {
		t.Error("empty query should match everything")
	}
}

func TestScore_Positions(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		candidate string
		wantPos   []int
	}{
		{"exact", "abc", "abc", []int{0, 1, 2}},
		{"prefix", "ab", "abcdef", []int{0, 1}},
		{"substring", "cd", "abcdef", []int{2, 3}},
		{"fuzzy", "adf", "abcdef", []int{0, 3, 5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Score(tt.query, tt.candidate)
			if m.Score == 0 {
				t.Fatal("expected a match")
			}
			if len(m.Positions) != len(tt.wantPos) {
				t.Fatalf("positions len = %d, want %d", len(m.Positions), len(tt.wantPos))
			}
			for i, pos := range m.Positions {
				if pos != tt.wantPos[i] {
					t.Errorf("positions[%d] = %d, want %d", i, pos, tt.wantPos[i])
				}
			}
		})
	}
}

func TestScore_ConsecutiveBonus(t *testing.T) {
	// "buf" as consecutive chars should score higher than "b_u_f" spread out.
	consecutive := Score("buf", "buffer.go")
	spread := Score("buf", "b_u_f_extra.go")
	if consecutive.Score <= spread.Score {
		t.Errorf("consecutive (%d) should score higher than spread (%d)",
			consecutive.Score, spread.Score)
	}
}

func TestScore_SeparatorBonus(t *testing.T) {
	// Match right after "/" should score higher than mid-word.
	afterSep := Score("session", "engine/session.go")
	midWord := Score("session", "obsession.go")
	if afterSep.Score <= midWord.Score {
		t.Errorf("after separator (%d) should score higher than mid-word (%d)",
			afterSep.Score, midWord.Score)
	}
}

func TestScore_ShorterCandidatePreferred(t *testing.T) {
	short := Score("main", "main.go")
	long := Score("main", "cmd/server/internal/main.go")
	if short.Score <= long.Score {
		t.Errorf("shorter candidate (%d) should score higher than longer (%d)",
			short.Score, long.Score)
	}
}

func TestScore_CamelCaseBonus(t *testing.T) {
	m := Score("NE", "NewEditor")
	if m.Score == 0 {
		t.Fatal("camelCase match should have score > 0")
	}
}

func TestFilter_RanksCorrectly(t *testing.T) {
	candidates := []string{
		"engine/session/session.go",
		"engine/session/session_test.go",
		"engine/editor/editor.go",
		"tui/internal/ui/editor.go",
		"session.go",
	}

	results := Filter("session", candidates)
	if len(results) == 0 {
		t.Fatal("expected matches")
	}
	// "session.go" (exact filename) should rank first — shortest, highest score.
	if results[0].Text != "session.go" {
		t.Errorf("first result = %q, want %q", results[0].Text, "session.go")
	}
}

func TestFilter_EmptyQuery(t *testing.T) {
	candidates := []string{"b.go", "a.go", "c.go"}
	results := Filter("", candidates)
	if len(results) != 3 {
		t.Fatalf("empty query should return all candidates, got %d", len(results))
	}
	// Should be sorted alphabetically.
	if results[0].Text != "a.go" {
		t.Errorf("first result = %q, want %q", results[0].Text, "a.go")
	}
}

func TestFilter_NoMatches(t *testing.T) {
	results := Filter("xyz", []string{"abc.go", "def.go"})
	if len(results) != 0 {
		t.Errorf("expected 0 matches, got %d", len(results))
	}
}

func TestFilter_NilCandidates(t *testing.T) {
	results := Filter("test", nil)
	if len(results) != 0 {
		t.Errorf("expected 0 matches for nil candidates, got %d", len(results))
	}
}

func TestScore_PathComponents(t *testing.T) {
	// Typing a path component should match well.
	m := Score("ui/app", "tui/internal/ui/app.go")
	if m.Score == 0 {
		t.Fatal("path component query should match")
	}
}

func TestScore_QueryLongerThanCandidate(t *testing.T) {
	m := Score("session.go.extra", "session.go")
	if m.Score != 0 {
		t.Errorf("query longer than candidate should not match, got score %d", m.Score)
	}
}

func TestScore_SingleChar(t *testing.T) {
	m := Score("s", "session.go")
	if m.Score == 0 {
		t.Fatal("single char query should match")
	}
}

func TestScore_Unicode(t *testing.T) {
	m := Score("日", "日本語.txt")
	if m.Score == 0 {
		t.Fatal("unicode query should match")
	}
}
