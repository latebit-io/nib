package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
)

func newTestEditor(content string) *editor.Editor {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	e := editor.New(buf)
	e.SetSize(80, 24)
	return e
}

func TestFindBarOpenClose(t *testing.T) {
	eng := newTestEditor("hello world")
	var fb FindBar

	fb.Open(eng, false)
	if !fb.Active {
		t.Fatal("expected find bar to be active after Open")
	}
	if fb.ReplaceMode {
		t.Fatal("expected replace mode to be false")
	}

	fb.Close()
	if fb.Active {
		t.Fatal("expected find bar to be inactive after Close")
	}
}

func TestFindBarOpenWithReplace(t *testing.T) {
	eng := newTestEditor("hello world")
	var fb FindBar

	fb.Open(eng, true)
	if !fb.ReplaceMode {
		t.Fatal("expected replace mode to be true")
	}
	fb.Close()
}

func TestFindBarOpenPreFillsSelection(t *testing.T) {
	eng := newTestEditor("hello world")
	eng.SelectionActive = true
	eng.SelectStartLine = 0
	eng.SelectStartCol = 0
	eng.CursorLine = 0
	eng.CursorCol = 5

	var fb FindBar
	fb.Open(eng, false)

	if string(fb.Query) != "hello" {
		t.Fatalf("expected query %q, got %q", "hello", string(fb.Query))
	}
	fb.Close()
}

func TestFindBarIncrementalSearch(t *testing.T) {
	eng := newTestEditor("foo bar foo baz foo")
	var fb FindBar
	fb.Open(eng, false)

	// Type "foo"
	fb.Update(tea.KeyPressMsg{Text: "f"})
	fb.Update(tea.KeyPressMsg{Text: "o"})
	fb.Update(tea.KeyPressMsg{Text: "o"})

	if len(fb.Matches) != 3 {
		t.Fatalf("expected 3 matches, got %d", len(fb.Matches))
	}
	if fb.CurrentMatch < 0 || fb.CurrentMatch >= len(fb.Matches) {
		t.Fatalf("expected valid current match index, got %d", fb.CurrentMatch)
	}
}

func TestFindBarNextPrev(t *testing.T) {
	eng := newTestEditor("aaa\naaa\naaa")
	var fb FindBar
	fb.Open(eng, false)

	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})

	if len(fb.Matches) != 3 {
		t.Fatalf("expected 3 matches, got %d", len(fb.Matches))
	}

	initial := fb.CurrentMatch

	// Next
	fb.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if fb.CurrentMatch != (initial+1)%3 {
		t.Fatalf("expected next match %d, got %d", (initial+1)%3, fb.CurrentMatch)
	}

	// Prev (Shift+Enter)
	fb.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	if fb.CurrentMatch != initial {
		t.Fatalf("expected match %d after prev, got %d", initial, fb.CurrentMatch)
	}
}

func TestFindBarWrapAround(t *testing.T) {
	eng := newTestEditor("abc\ndef\nabc")
	var fb FindBar
	fb.Open(eng, false)

	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "b"})
	fb.Update(tea.KeyPressMsg{Text: "c"})

	if len(fb.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(fb.Matches))
	}

	// Navigate past last match — should wrap to first.
	fb.NextMatch()
	fb.NextMatch()
	if fb.CurrentMatch != 0 {
		t.Fatalf("expected wrap to match 0, got %d", fb.CurrentMatch)
	}

	// Navigate backwards past first — should wrap to last.
	fb.PrevMatch()
	if fb.CurrentMatch != 1 {
		t.Fatalf("expected wrap to match 1, got %d", fb.CurrentMatch)
	}
}

func TestFindBarCaseSensitivity(t *testing.T) {
	eng := newTestEditor("Hello HELLO hello")
	var fb FindBar
	fb.Open(eng, false)

	// Default: case insensitive
	fb.Update(tea.KeyPressMsg{Text: "h"})
	fb.Update(tea.KeyPressMsg{Text: "e"})
	fb.Update(tea.KeyPressMsg{Text: "l"})
	fb.Update(tea.KeyPressMsg{Text: "l"})
	fb.Update(tea.KeyPressMsg{Text: "o"})

	if len(fb.Matches) != 3 {
		t.Fatalf("case insensitive: expected 3 matches, got %d", len(fb.Matches))
	}

	// Toggle case sensitive (Alt+C)
	fb.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})

	if !fb.CaseSensitive {
		t.Fatal("expected case sensitive to be true")
	}
	if len(fb.Matches) != 1 {
		t.Fatalf("case sensitive: expected 1 match for 'hello', got %d", len(fb.Matches))
	}
}

func TestFindBarBackspace(t *testing.T) {
	eng := newTestEditor("foobar")
	var fb FindBar
	fb.Open(eng, false)

	fb.Update(tea.KeyPressMsg{Text: "f"})
	fb.Update(tea.KeyPressMsg{Text: "o"})
	fb.Update(tea.KeyPressMsg{Text: "o"})

	if string(fb.Query) != "foo" {
		t.Fatalf("expected query 'foo', got %q", string(fb.Query))
	}

	fb.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if string(fb.Query) != "fo" {
		t.Fatalf("after backspace expected 'fo', got %q", string(fb.Query))
	}
}

func TestFindBarEscapeRestoresCursor(t *testing.T) {
	eng := newTestEditor("aaa\nbbb\naaa")
	eng.MoveCursorTo(1, 1) // middle of file

	var fb FindBar
	fb.Open(eng, false)

	// Type a query that matches first and third lines
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})

	// Cursor should have moved to a match
	if eng.CursorLine == 1 {
		t.Fatal("expected cursor to move away from line 1")
	}

	// Close without navigating — cursor should restore
	fb.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	if eng.CursorLine != 1 || eng.CursorCol != 1 {
		t.Fatalf("expected cursor restored to (1,1), got (%d,%d)", eng.CursorLine, eng.CursorCol)
	}
}

func TestFindBarNoMatchesEmptyQuery(t *testing.T) {
	eng := newTestEditor("hello world")
	var fb FindBar
	fb.Open(eng, false)

	if len(fb.Matches) != 0 {
		t.Fatalf("expected 0 matches for empty query, got %d", len(fb.Matches))
	}
	if fb.CurrentMatch != -1 {
		t.Fatalf("expected current match -1, got %d", fb.CurrentMatch)
	}
}

func TestFindBarIsMatchAt(t *testing.T) {
	eng := newTestEditor("hello world hello")
	var fb FindBar
	fb.Open(eng, false)

	fb.Update(tea.KeyPressMsg{Text: "h"})
	fb.Update(tea.KeyPressMsg{Text: "e"})
	fb.Update(tea.KeyPressMsg{Text: "l"})
	fb.Update(tea.KeyPressMsg{Text: "l"})
	fb.Update(tea.KeyPressMsg{Text: "o"})

	// First match at col 0-4
	isMatch, isCurrent := fb.IsMatchAt(0, 0)
	if !isMatch {
		t.Fatal("expected match at (0, 0)")
	}
	if !isCurrent {
		t.Fatal("expected current match at (0, 0)")
	}

	// Second match at col 12-16
	isMatch, isCurrent = fb.IsMatchAt(0, 12)
	if !isMatch {
		t.Fatal("expected match at (0, 12)")
	}
	if isCurrent {
		t.Fatal("second match should not be current")
	}

	// No match at col 6 (inside "world")
	isMatch, _ = fb.IsMatchAt(0, 6)
	if isMatch {
		t.Fatal("expected no match at (0, 6)")
	}
}

func TestFindBarReplace(t *testing.T) {
	eng := newTestEditor("aaa bbb aaa")
	var fb FindBar
	fb.Open(eng, true)

	// Search for "aaa"
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})

	if len(fb.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(fb.Matches))
	}

	// Switch to replace field
	fb.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if !fb.ReplaceActive {
		t.Fatal("expected replace field to be active")
	}

	// Type replacement
	fb.Update(tea.KeyPressMsg{Text: "x"})
	fb.Update(tea.KeyPressMsg{Text: "x"})

	// Replace current
	fb.ReplaceCurrent()

	content := eng.Buf.LineText(0)
	if content != "xx bbb aaa" {
		t.Fatalf("after replace, expected 'xx bbb aaa', got %q", content)
	}
}

func TestFindBarReplaceAll(t *testing.T) {
	eng := newTestEditor("aaa bbb aaa ccc aaa")
	var fb FindBar
	fb.Open(eng, true)

	// Search
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})

	// Set replace text
	fb.ReplaceQuery = []rune("ZZ")
	fb.ReplaceCursor = 2

	fb.ReplaceAll()

	content := eng.Buf.LineText(0)
	if content != "ZZ bbb ZZ ccc ZZ" {
		t.Fatalf("after replace all, expected 'ZZ bbb ZZ ccc ZZ', got %q", content)
	}
}

// Regression: overlapping matches (e.g. "aa" in "aaa") must not corrupt text
// during replace-all. Only non-overlapping matches should be replaced.
func TestFindBarReplaceAllOverlapping(t *testing.T) {
	eng := newTestEditor("aaa")
	var fb FindBar
	fb.Open(eng, true)

	// Search for "aa" — overlapping matches at col 0 and col 1
	fb.Update(tea.KeyPressMsg{Text: "a"})
	fb.Update(tea.KeyPressMsg{Text: "a"})

	if len(fb.Matches) != 2 {
		t.Fatalf("expected 2 overlapping matches, got %d", len(fb.Matches))
	}

	fb.ReplaceQuery = []rune("X")
	fb.ReplaceCursor = 1

	fb.ReplaceAll()

	got := eng.Buf.LineText(0)
	// Only the first non-overlapping match (col 0) should be replaced.
	// "aa" at col 0 → "X", leaving "Xa" (not "XX" or corrupted text).
	if got != "Xa" {
		t.Fatalf("after replace all with overlapping matches, expected 'Xa', got %q", got)
	}
}

// Regression: replacing "todo" with "todos" must not re-match the "todo"
// inside "todos" on subsequent Enter presses.
func TestFindBarReplaceDoesNotRematch(t *testing.T) {
	eng := newTestEditor("todo and todo")
	var fb FindBar
	fb.Open(eng, true)

	// Search for "todo"
	fb.Update(tea.KeyPressMsg{Text: "t"})
	fb.Update(tea.KeyPressMsg{Text: "o"})
	fb.Update(tea.KeyPressMsg{Text: "d"})
	fb.Update(tea.KeyPressMsg{Text: "o"})

	if len(fb.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(fb.Matches))
	}

	// Switch to replace and type "todos"
	fb.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	fb.Update(tea.KeyPressMsg{Text: "t"})
	fb.Update(tea.KeyPressMsg{Text: "o"})
	fb.Update(tea.KeyPressMsg{Text: "d"})
	fb.Update(tea.KeyPressMsg{Text: "o"})
	fb.Update(tea.KeyPressMsg{Text: "s"})

	// Replace first occurrence.
	fb.ReplaceCurrent()
	got := eng.Buf.LineText(0)
	if got != "todos and todo" {
		t.Fatalf("after first replace, expected 'todos and todo', got %q", got)
	}

	// Current match should now point at the second "todo", not re-match
	// the "todo" inside "todos".
	if fb.CurrentMatch < 0 || fb.CurrentMatch >= len(fb.Matches) {
		t.Fatalf("expected valid current match, got %d (of %d)", fb.CurrentMatch, len(fb.Matches))
	}
	cur := fb.Matches[fb.CurrentMatch]
	if cur.Col < 5 {
		t.Fatalf("current match should be past the replacement (col >= 5), got col %d", cur.Col)
	}

	// Replace second occurrence.
	fb.ReplaceCurrent()
	got = eng.Buf.LineText(0)
	if got != "todos and todos" {
		t.Fatalf("after second replace, expected 'todos and todos', got %q", got)
	}

	// Pressing replace again should NOT create "todoss" — the only matches
	// left are inside the replacements, so text must stay unchanged.
	fb.ReplaceCurrent()
	got = eng.Buf.LineText(0)
	if got != "todos and todos" {
		t.Fatalf("third replace should leave text unchanged, got %q", got)
	}
}

func TestFindBarRender(t *testing.T) {
	eng := newTestEditor("hello world")
	var fb FindBar
	fb.Open(eng, false)

	fb.Update(tea.KeyPressMsg{Text: "h"})
	fb.Update(tea.KeyPressMsg{Text: "e"})
	fb.Update(tea.KeyPressMsg{Text: "l"})

	lines := fb.Render(80)
	if len(lines) != 1 {
		t.Fatalf("expected 1 render line, got %d", len(lines))
	}
	if len(lines[0]) == 0 {
		t.Fatal("expected non-empty render output")
	}
}

func TestFindBarHeight(t *testing.T) {
	var fb FindBar
	if fb.Height() != 0 {
		t.Fatal("inactive find bar should have height 0")
	}

	eng := newTestEditor("test")
	fb.Open(eng, false)
	if fb.Height() != 1 {
		t.Fatal("active find bar should have height 1")
	}

	fb.Close()
	fb.Open(eng, true)
	if fb.Height() != 2 {
		t.Fatal("active find+replace bar should have height 2")
	}
}
