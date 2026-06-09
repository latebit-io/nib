// Package frontmatter splits a leading YAML frontmatter block from a
// markdown document body. It is the one place nib parses the
// `---` … `---` header convention, shared by the slash-command loader
// (kit/command/loader) and the skill loader (kit/skill) so the two
// agree on exactly what counts as frontmatter.
package frontmatter

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const fence = "---"

// utf8BOM is stripped before fence detection so a Windows-injected BOM
// does not hide the leading `---`.
const utf8BOM = "\xef\xbb\xbf"

// Split extracts a YAML frontmatter block (if present) from a markdown
// document's bytes, unmarshals it into out, and returns the remaining
// body. out must be a non-nil pointer to a struct with yaml tags;
// callers that want only the body may pass a pointer to an empty
// struct.
//
// Documents without a frontmatter block are valid: the whole content
// becomes the body, out is left untouched (its zero value), and err is
// nil. Frontmatter is detected by a leading `---` line; the closing
// `---` must appear on its own line. An in-body `---` horizontal rule
// on a document with no opening fence is left in the body. A file that
// opens a fence but never closes it is treated leniently as
// all-body — matching the "missing close means no frontmatter" policy
// the command loader established.
//
// Returns an error only when the frontmatter block is present but is
// malformed YAML.
func Split(data []byte, out any) (body string, err error) {
	text := strings.TrimPrefix(string(data), utf8BOM)

	if !strings.HasPrefix(text, fence) {
		return text, nil
	}
	// First line must be exactly "---" (trailing whitespace allowed,
	// no other content). A `---` with content on the same line is body.
	rest := text[len(fence):]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 || strings.TrimSpace(rest[:nl]) != "" {
		return text, nil
	}
	rest = rest[nl+1:]

	var fmText string
	for {
		eol := strings.IndexByte(rest, '\n')
		line := rest
		if eol >= 0 {
			line = rest[:eol]
		}
		if strings.TrimSpace(line) == fence {
			b := ""
			if eol >= 0 {
				b = rest[eol+1:]
			}
			if strings.TrimSpace(fmText) != "" {
				if err := yaml.Unmarshal([]byte(fmText), out); err != nil {
					return "", fmt.Errorf("frontmatter: %w", err)
				}
			}
			return b, nil
		}
		fmText += line + "\n"
		if eol < 0 {
			// EOF without a closing fence — treat the whole document as
			// body, matching the lenient policy.
			return text, nil
		}
		rest = rest[eol+1:]
	}
}
