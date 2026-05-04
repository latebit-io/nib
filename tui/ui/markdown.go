package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// Package-level markdown styles — allocated once, never inside render paths.
var (
	mdCodeBlockStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("78")).Background(lipgloss.Color("236"))
	mdInlineCodeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	mdBoldStyle       = lipgloss.NewStyle().Bold(true)
	mdItalicStyle     = lipgloss.NewStyle().Italic(true)
	mdBoldItalicStyle = lipgloss.NewStyle().Bold(true).Italic(true)
	// mdHeaderStyle renders # H1 as a filled bar — bright fg on the same
	// purple bg used by the THINKING status chip so headings read as
	// structural sections, not just bigger text.
	mdHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("55"))
	mdHeader2Style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	mdHeader3Style = lipgloss.NewStyle().Bold(true)
)

// mdKind identifies the kind of an inline markdown span.
type mdKind int

const (
	mdPlain      mdKind = iota
	mdCode              // `code`
	mdBold              // **bold**
	mdItalic            // *italic*
	mdBoldItalic        // ***bold italic***
)

// mdSpan is a segment of inline-parsed text with its rendering kind.
// The text field always includes delimiter characters (backticks, asterisks)
// so that span rune counts equal the raw source rune counts. This keeps
// display cell positions aligned with m.Lines rune indices, preserving
// correctness of mouse hit-testing and selection without a display→source map.
type mdSpan struct {
	text string
	kind mdKind
}

// renderMarkdownLine renders a single agent pane line with markdown styling and
// pads the result to exactly width display cells.
//
// Marker characters (# prefixes, **, *, `, bullet replacements) are KEPT in
// the rendered output so display cell columns equal raw m.Lines rune indices.
// This preserves mouseToLineCol, isSelected, and SelectedText correctness.
// isCode indicates the line is inside (or is) a code block fence.
func renderMarkdownLine(line string, isCode bool, width int) string {
	if isCode {
		return mdCodeBlockStyle.Render(padToWidth(line, width))
	}

	// Headers: style the full line including `# ` prefix — no stripping.
	switch {
	case strings.HasPrefix(line, "### "):
		return mdHeader3Style.Render(padToWidth(line, width))
	case strings.HasPrefix(line, "## "):
		return mdHeader2Style.Render(padToWidth(line, width))
	case strings.HasPrefix(line, "# "):
		return mdHeaderStyle.Render(padToWidth(line, width))
	}

	// Bullets: replace leading -, +, or * marker with • (U+2022).
	// All three markers are single display cells, so rune positions after the
	// marker are unchanged. * at line start followed by space is unambiguous —
	// italic requires *text* with a closing marker.
	trimmed := strings.TrimLeft(line, " \t")
	indent := len([]rune(line)) - len([]rune(trimmed))
	if len(trimmed) >= 2 && (trimmed[0] == '-' || trimmed[0] == '+' || trimmed[0] == '*') && trimmed[1] == ' ' {
		bullet := strings.Repeat(" ", indent) + "• " + trimmed[2:]
		return applyInlineMarkdown(bullet, width)
	}

	return applyInlineMarkdown(line, width)
}

// padToWidth returns s padded with spaces so its display width equals width.
// If s is already at or above width it is returned unchanged.
func padToWidth(s string, width int) string {
	w := runewidth.StringWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// applyInlineMarkdown parses inline markdown spans in line, applies lipgloss
// styles, and returns the result padded to width display cells.
func applyInlineMarkdown(line string, width int) string {
	spans := parseInlineMarkdown(line)
	var sb strings.Builder
	plainWidth := 0
	for _, span := range spans {
		plainWidth += runewidth.StringWidth(span.text)
		switch span.kind {
		case mdCode:
			sb.WriteString(mdInlineCodeStyle.Render(span.text))
		case mdBold:
			sb.WriteString(mdBoldStyle.Render(span.text))
		case mdItalic:
			sb.WriteString(mdItalicStyle.Render(span.text))
		case mdBoldItalic:
			sb.WriteString(mdBoldItalicStyle.Render(span.text))
		default:
			sb.WriteString(span.text)
		}
	}
	if plainWidth < width {
		sb.WriteString(strings.Repeat(" ", width-plainWidth))
	}
	return sb.String()
}

// parseInlineMarkdown splits text into styled spans for inline rendering.
// Handles: `code`, **bold**, *italic*, ***bold italic***.
//
// Span text INCLUDES delimiter characters so that the total rune count of all
// spans equals the input rune count. This is the invariant that keeps display
// cell columns aligned with raw source column indices.
//
// All indexing is done on []rune, never via strings.Index or byte offsets,
// to avoid the rune/byte mismatch bug documented in patterns.md.
func parseInlineMarkdown(text string) []mdSpan {
	runes := []rune(text)
	n := len(runes)
	var spans []mdSpan
	var plain strings.Builder
	i := 0

	flushPlain := func() {
		if plain.Len() > 0 {
			spans = append(spans, mdSpan{plain.String(), mdPlain})
			plain.Reset()
		}
	}

	for i < n {
		r := runes[i]

		// Inline code: `content` — span includes both backticks.
		if r == '`' {
			j := i + 1
			for j < n && runes[j] != '`' {
				j++
			}
			if j < n { // found closing backtick
				flushPlain()
				spans = append(spans, mdSpan{string(runes[i : j+1]), mdCode})
				i = j + 1
				continue
			}
			// No closing backtick — emit as plain.
			plain.WriteRune(r)
			i++
			continue
		}

		// Bold / italic: *, **, *** — span includes all delimiter runes.
		if r == '*' {
			// Count the run of stars starting at i.
			j := i + 1
			for j < n && runes[j] == '*' {
				j++
			}
			stars := j - i
			if stars <= 3 {
				// Find a matching closing run of exactly the same length.
				closeIdx := findRuneSeq(runes, j, '*', stars)
				if closeIdx >= 0 {
					flushPlain()
					// Include opening markers (i..j) + content + closing markers.
					content := string(runes[i : closeIdx+stars])
					var kind mdKind
					switch stars {
					case 1:
						kind = mdItalic
					case 2:
						kind = mdBold
					default:
						kind = mdBoldItalic
					}
					spans = append(spans, mdSpan{content, kind})
					i = closeIdx + stars
					continue
				}
			}
			// No valid closing — emit star run as plain.
			plain.WriteString(string(runes[i:j]))
			i = j
			continue
		}

		plain.WriteRune(r)
		i++
	}

	flushPlain()
	return spans
}

// findRuneSeq scans runes[start:] for a run of exactly count consecutive
// occurrences of r that is not longer than count. Returns the start index of
// the first such run, or -1 if none is found.
func findRuneSeq(runes []rune, start int, r rune, count int) int {
	n := len(runes)
	for i := start; i < n; {
		if runes[i] != r {
			i++
			continue
		}
		j := i + 1
		for j < n && runes[j] == r {
			j++
		}
		if j-i == count {
			return i
		}
		i = j
	}
	return -1
}

// --- Hover markdown rendering (used by the editor's hover overlay) ---

// renderHoverMarkdown converts gopls markdown hover output into styled
// terminal text. Handles code fences (colored), horizontal rules (dim
// separator), bold markers, and inline code spans.
func renderHoverMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	inCode := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Code fence toggle.
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			continue
		}

		// Horizontal rule → dim separator.
		if trimmed == "---" || trimmed == "___" || trimmed == "***" {
			out = append(out, hoverDimStyle.Render("─────"))
			continue
		}

		if inCode {
			out = append(out, hoverCodeStyle.Render(line))
		} else if trimmed == "" {
			out = append(out, "")
		} else {
			// Render inline markdown: **bold** and `code`.
			rendered := renderInlineMarkdown(line)
			out = append(out, rendered)
		}
	}

	// Trim leading/trailing blank lines.
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// renderInlineMarkdown handles **bold** and `code` spans in a single line.
func renderInlineMarkdown(s string) string {
	var b strings.Builder
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		// Bold: **text**
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			end := -1
			for j := i + 2; j+1 < len(runes); j++ {
				if runes[j] == '*' && runes[j+1] == '*' {
					end = j
					break
				}
			}
			if end >= 0 {
				b.WriteString(hoverBoldStyle.Render(string(runes[i+2 : end])))
				i = end + 2
				continue
			}
		}
		// Inline code: `text`
		if runes[i] == '`' {
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '`' {
					end = j
					break
				}
			}
			if end >= 0 {
				b.WriteString(hoverCodeStyle.Render(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}
