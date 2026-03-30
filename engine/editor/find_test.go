package editor

import (
	"testing"
)

func assertFindAll(t *testing.T, content, query string, caseSensitive bool, want []FindMatch) {
	t.Helper()
	e := newLineOpsEditor(content)
	got := e.FindAll(query, caseSensitive)
	if len(got) != len(want) {
		t.Fatalf("FindAll(%q, %v) returned %d matches, want %d\ngot:  %v\nwant: %v",
			query, caseSensitive, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("match[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFindAllEmpty(t *testing.T) {
	assertFindAll(t, "hello world", "", false, nil)
}

func TestFindAllNoMatch(t *testing.T) {
	assertFindAll(t, "hello world", "xyz", false, nil)
}

func TestFindAllSingle(t *testing.T) {
	assertFindAll(t, "hello world", "world", false, []FindMatch{{Line: 0, Col: 6, Len: 5}})
}

func TestFindAllMultipleSameLine(t *testing.T) {
	assertFindAll(t, "abcabc", "abc", false, []FindMatch{
		{Line: 0, Col: 0, Len: 3},
		{Line: 0, Col: 3, Len: 3},
	})
}

func TestFindAllAcrossLines(t *testing.T) {
	assertFindAll(t, "foo bar\nbaz foo\nfoo", "foo", false, []FindMatch{
		{Line: 0, Col: 0, Len: 3},
		{Line: 1, Col: 4, Len: 3},
		{Line: 2, Col: 0, Len: 3},
	})
}

func TestFindAllCaseInsensitive(t *testing.T) {
	assertFindAll(t, "Hello HELLO hello", "hello", false, []FindMatch{
		{Line: 0, Col: 0, Len: 5},
		{Line: 0, Col: 6, Len: 5},
		{Line: 0, Col: 12, Len: 5},
	})
}

func TestFindAllCaseSensitive(t *testing.T) {
	assertFindAll(t, "Hello HELLO hello", "Hello", true, []FindMatch{{Line: 0, Col: 0, Len: 5}})
}

func TestFindAllUnicode(t *testing.T) {
	assertFindAll(t, "日本語テスト", "テスト", false, []FindMatch{{Line: 0, Col: 3, Len: 3}})
}

func TestFindAllOverlapping(t *testing.T) {
	assertFindAll(t, "aaa", "aa", false, []FindMatch{
		{Line: 0, Col: 0, Len: 2},
		{Line: 0, Col: 1, Len: 2},
	})
}
