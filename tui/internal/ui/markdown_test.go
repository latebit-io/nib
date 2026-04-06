package ui

import (
	"strings"
	"testing"
)

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
			name:  "inline code",
			input: "use `foo()` here",
			want:  []mdSpan{{"use ", mdPlain}, {"foo()", mdCode}, {" here", mdPlain}},
		},
		{
			name:  "bold",
			input: "this is **bold** text",
			want:  []mdSpan{{"this is ", mdPlain}, {"bold", mdBold}, {" text", mdPlain}},
		},
		{
			name:  "italic",
			input: "this is *italic* text",
			want:  []mdSpan{{"this is ", mdPlain}, {"italic", mdItalic}, {" text", mdPlain}},
		},
		{
			name:  "bold italic",
			input: "***both***",
			want:  []mdSpan{{"both", mdBoldItalic}},
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
		{"header", "# Title", false},
		{"bullet", "- item", false},
		{"code block", "func foo() {}", true},
		{"inline code", "use `x` here", false},
		{"bold", "**bold** text", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderMarkdownLine(tc.line, tc.isCode, width)
			// Strip ANSI escape sequences to measure visual width.
			plain := stripANSI(out)
			if len([]rune(plain)) != width {
				t.Errorf("renderMarkdownLine(%q, %v, %d): visual width %d, want %d (plain=%q)",
					tc.line, tc.isCode, width, len([]rune(plain)), width, plain)
			}
		})
	}
}

func TestAgentPaneModel_isCodeLine(t *testing.T) {
	m := &AgentPaneModel{Width: 80, Height: 20}
	// Simulate Lines from a code block.
	m.Lines = []string{
		"before",
		"```go",
		"func foo() {}",
		"```",
		"after",
	}
	m.recomputeCodeBlock(0)

	want := []bool{false, true, true, true, false}
	for i, w := range want {
		got := m.isCodeLine(i)
		if got != w {
			t.Errorf("isCodeLine(%d)=%v want %v (line=%q)", i, got, w, m.Lines[i])
		}
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
