package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

type testClipboard struct{ content string }

func (c *testClipboard) Read() string         { return c.content }
func (c *testClipboard) Write(s string) error { c.content = s; return nil }

func TestParseInlineMarkdown(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []mdSpan
	}{
		{
			name:  "plain text",
			input: "hello world",
			want:  []mdSpan{{"hello world", mdPlain}},
		},
		{
			// Span text includes both backticks so rune count is preserved.
			name:  "inline code",
			input: "use `foo()` here",
			want:  []mdSpan{{"use ", mdPlain}, {"`foo()`", mdCode}, {" here", mdPlain}},
		},
		{
			// Span text includes ** delimiters on both sides.
			name:  "bold",
			input: "this is **bold** text",
			want:  []mdSpan{{"this is ", mdPlain}, {"**bold**", mdBold}, {" text", mdPlain}},
		},
		{
			// Span text includes * delimiters on both sides.
			name:  "italic",
			input: "this is *italic* text",
			want:  []mdSpan{{"this is ", mdPlain}, {"*italic*", mdItalic}, {" text", mdPlain}},
		},
		{
			// Span text includes *** delimiters on both sides.
			name:  "bold italic",
			input: "***both***",
			want:  []mdSpan{{"***both***", mdBoldItalic}},
		},
		{
			name:  "unclosed backtick treated as plain",
			input: "no `close here",
			want:  []mdSpan{{"no `close here", mdPlain}},
		},
		{
			name:  "unclosed stars treated as plain",
			input: "**no close",
			want:  []mdSpan{{"**no close", mdPlain}},
		},
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseInlineMarkdown(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d spans, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, s := range got {
				if s.text != tt.want[i].text || s.kind != tt.want[i].kind {
					t.Errorf("span[%d]: got {%q, %d} want {%q, %d}", i, s.text, s.kind, tt.want[i].text, tt.want[i].kind)
				}
			}
		})
	}
}

// TestParseInlineMarkdown_RuneCountInvariant verifies the key property that
// preserves hit-test correctness: the sum of span rune counts equals the
// input rune count, so display cell columns == raw m.Lines rune indices.
func TestParseInlineMarkdown_RuneCountInvariant(t *testing.T) {
	inputs := []string{
		"hello world",
		"use `foo()` here",
		"this is **bold** text",
		"this is *italic* text",
		"***both***",
		"no `close here",
		"**no close",
		"mix `code` and **bold** together",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			wantRunes := utf8.RuneCountInString(input)
			spans := parseInlineMarkdown(input)
			got := 0
			for _, s := range spans {
				got += utf8.RuneCountInString(s.text)
			}
			if got != wantRunes {
				t.Errorf("rune count: got %d want %d for %q", got, wantRunes, input)
			}
		})
	}
}

func TestRenderMarkdownLine_Width(t *testing.T) {
	// Output must be padded to exactly the requested width (in display cells).
	// ANSI codes don't count toward visual width, so we strip them when measuring.
	width := 20
	cases := []struct {
		name   string
		line   string
		isCode bool
	}{
		{"plain", "hello", false},
		{"header h1", "# Title", false},
		{"header h2", "## Section", false},
		{"header h3", "### Sub", false},
		{"bullet dash", "- item", false},
		{"bullet plus", "+ item", false},
		{"bullet star", "* item", false},
		{"code block", "func foo() {}", true},
		{"inline code", "use `x` here", false},
		{"bold", "**bold** text", false},
		{"italic", "*italic* text", false},
		{"wide CJK", "你好world", false},
		{"wide emoji", "🎉 done", false},
		{"wide code block", "你好", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderMarkdownLine(tc.line, tc.isCode, width)
			plain := stripANSI(out)
			if runewidth.StringWidth(plain) != width {
				t.Errorf("renderMarkdownLine(%q, %v, %d): visual width %d, want %d (plain=%q)",
					tc.line, tc.isCode, width, runewidth.StringWidth(plain), width, plain)
			}
		})
	}
}

// TestRenderMarkdownLine_ColumnAlignment verifies that the plain-text content
// of a rendered line has the same rune count as the source line (headers
// included, because we no longer strip `# ` prefixes). This is the property
// that lets mouseToLineCol operate on m.Lines without a display→source map.
func TestRenderMarkdownLine_ColumnAlignment(t *testing.T) {
	cases := []struct{ line string }{
		{"# Header one"},
		{"## Header two"},
		{"### Header three"},
		{"plain text here"},
		{"use `code` inline"},
		{"**bold** and *italic*"},
		{"- bullet item"},
		{"* star bullet"},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			// Render to a width larger than any test input so no truncation occurs.
			out := renderMarkdownLine(tc.line, false, 80)
			plainRendered := strings.TrimRight(stripANSI(out), " ")
			wantRunes := utf8.RuneCountInString(tc.line)
			gotRunes := utf8.RuneCountInString(plainRendered)
			if gotRunes != wantRunes {
				t.Errorf("%q: rendered rune count %d, source rune count %d — column alignment broken",
					tc.line, gotRunes, wantRunes)
			}
		})
	}
}

func TestAgentPaneModel_isCodeLine(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(80, 20)
	m.AppendText("before\n```go\nfunc foo() {}\n```\nafter")

	want := []bool{false, true, true, true, false}
	if len(m.Lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(m.Lines), len(want), m.Lines)
	}
	for i, w := range want {
		got := m.isCodeLine(i)
		if got != w {
			t.Errorf("isCodeLine(%d)=%v want %v (line=%q)", i, got, w, m.Lines[i])
		}
	}
}

// TestAgentPaneModel_isCodeLine_NarrowWidth verifies that fence detection works
// from RawLines even when the fence line wraps at narrow widths. A width of 2
// would split "```go" into multiple wrapped lines — none of which start with
// "```" — so scanning wrapped Lines for fences would miss it entirely.
func TestAgentPaneModel_isCodeLine_NarrowWidth(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(4, 20) // narrow: "```go" wraps to ["```g", "o"]
	m.AppendText("hi\n```go\nfunc foo() {}\n```\nbye")

	// Every wrapped line belonging to a code-block raw line must be flagged.
	for i, line := range m.Lines {
		got := m.isCodeLine(i)

		// Determine which raw line owns this wrapped line.
		rawIdx := 0
		for ri := len(m.wrappedIndex) - 1; ri >= 0; ri-- {
			if m.wrappedIndex[ri] <= i {
				rawIdx = ri
				break
			}
		}
		raw := m.RawLines[rawIdx]

		// "hi" and "bye" are outside the block; everything else is inside.
		wantCode := raw != "hi" && raw != "bye"
		if got != wantCode {
			t.Errorf("isCodeLine(%d)=%v want %v (wrapped=%q, raw=%q)", i, got, wantCode, line, raw)
		}
	}
}

// TestAgentPaneModel_isCodeLine_WrappedCloser verifies that all wrapped segments
// of a closing fence are marked as code. At width 2, "```" wraps to ["“", "`"]
// and both segments must be code-styled.
func TestAgentPaneModel_isCodeLine_WrappedCloser(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(2, 20)
	m.AppendText("x\n```\ny\n```\nz")

	for i, line := range m.Lines {
		got := m.isCodeLine(i)

		rawIdx := 0
		for ri := len(m.wrappedIndex) - 1; ri >= 0; ri-- {
			if m.wrappedIndex[ri] <= i {
				rawIdx = ri
				break
			}
		}
		raw := m.RawLines[rawIdx]

		wantCode := raw != "x" && raw != "z"
		if got != wantCode {
			t.Errorf("isCodeLine(%d)=%v want %v (wrapped=%q, raw=%q)", i, got, wantCode, line, raw)
		}
	}
}

// TestAgentPaneModel_isCodeLine_NestedFence verifies that an inner ``` fence
// inside a ```“ block does not close the outer block.
func TestAgentPaneModel_isCodeLine_NestedFence(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(80, 30)
	m.AppendText("before\n`````\ninner ```\nstill code\n`````\nafter")

	// RawLines: "before", "`````", "inner ```", "still code", "`````", "after"
	// Only "before" and "after" are outside the block.
	for i := range m.Lines {
		got := m.isCodeLine(i)
		line := m.Lines[i]
		wantCode := line != "before" && line != "after"
		if got != wantCode {
			t.Errorf("nested: isCodeLine(%d)=%v want %v (line=%q)", i, got, wantCode, line)
		}
	}
}

// TestAgentPaneModel_isCodeLine_TildeFence verifies tilde fences work.
func TestAgentPaneModel_isCodeLine_TildeFence(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(80, 20)
	m.AppendText("before\n~~~\ncode\n~~~\nafter")

	want := map[string]bool{
		"before": false,
		"~~~":    true,
		"code":   true,
		"after":  false,
	}
	for i, line := range m.Lines {
		got := m.isCodeLine(i)
		// ~~~ appears twice (opener and closer); both should be code.
		if w, ok := want[line]; ok && got != w {
			t.Errorf("tilde: isCodeLine(%d)=%v want %v (line=%q)", i, got, w, line)
		}
	}
}

// TestAgentPaneModel_isCodeLine_UserFenceNoBleed verifies that an unmatched
// fence in a user message does not bleed into subsequent agent output.
func TestAgentPaneModel_isCodeLine_UserFenceNoBleed(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(80, 30)

	// Agent writes some text, then user sends a message with an unmatched fence.
	m.AppendText("agent line 1")
	m.AppendUserMessage("here is a fence: ```")
	m.AppendText("agent line 2\nagent line 3")

	// Agent lines after the user message must NOT be marked as code.
	for i, line := range m.Lines {
		isUser := m.isUserLine(i)
		isCode := m.isCodeLine(i)
		if !isUser && isCode {
			t.Errorf("isCodeLine(%d)=%v — user fence bled into agent line %q", i, isCode, line)
		}
	}
}

// TestAgentPaneModel_isCodeLine_TrailingTextDoesNotCloseFence verifies that a
// fence-like line with trailing non-space content (e.g. "```go" inside an open
// block) does NOT close the block. Per CommonMark, a closing fence must have
// only optional trailing spaces.
func TestAgentPaneModel_isCodeLine_TrailingTextDoesNotCloseFence(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &testClipboard{}})
	m.SetSize(80, 20)
	m.AppendText("before\n```\n```go\nstill code\n```\nafter")

	// Lines: "before", "```"(opener), "```go"(body, NOT closer), "still code", "```"(closer), "after"
	want := []bool{false, true, true, true, true, false}
	if len(m.Lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(m.Lines), len(want), m.Lines)
	}
	for i, w := range want {
		if got := m.isCodeLine(i); got != w {
			t.Errorf("isCodeLine(%d)=%v want %v (line=%q)", i, got, w, m.Lines[i])
		}
	}
}

// TestParseFenceLine verifies fence detection edge cases.
func TestParseFenceLine(t *testing.T) {
	tests := []struct {
		line         string
		wantChar     rune
		wantLen      int
		wantCanClose bool
	}{
		{"```", '`', 3, true},
		{"```go", '`', 3, false}, // info string → can open, NOT close
		{"`````", '`', 5, true},
		{"~~~", '~', 3, true},
		{"~~~~python", '~', 4, false}, // info string → can open, NOT close
		{"  ```", '`', 3, true},       // 2 leading spaces OK
		{"   ```", '`', 3, true},      // 3 leading spaces OK
		{"    ```", 0, 0, false},      // 4 leading spaces — NOT a fence
		{"``", 0, 0, false},           // too short
		{"not a fence", 0, 0, false},  // no fence chars
		{"", 0, 0, false},             // empty
		{"```~``", '`', 3, false},     // trailing non-space → can open, NOT close
		{"```   ", '`', 3, true},      // trailing spaces only → can close
		{"```\t", '`', 3, true},       // trailing tab only → can close
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			ch, n, canClose := parseFenceLine(tt.line)
			if ch != tt.wantChar || n != tt.wantLen || canClose != tt.wantCanClose {
				t.Errorf("parseFenceLine(%q) = (%c, %d, %v), want (%c, %d, %v)",
					tt.line, ch, n, canClose, tt.wantChar, tt.wantLen, tt.wantCanClose)
			}
		})
	}
}

// stripANSI removes ANSI escape sequences from s for visual-width measurement.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// skip
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
