// Package fence puts untrusted text into the system prompt inside an
// element it cannot close, so none of it reads as sitting outside.
package fence

import (
	"fmt"
	"html"
	"regexp"
)

// Fence is one kind of element: a tag and the attribute that names
// what each element holds.
type Fence struct {
	tag, attr string
	closing   *regexp.Regexp
}

// New returns the fence for tag, with attr naming each element.
func New(tag, attr string) Fence {
	// A closing tag in any case and spacing a model would still read as one.
	closing := regexp.MustCompile(`(?i)<\s*/\s*` + regexp.QuoteMeta(tag) + `\s*>`)
	return Fence{tag: tag, attr: attr, closing: closing}
}

// Wrap returns content inside the element, its attribute set to value.
// A closing tag in content is escaped, and so is the value.
func (f Fence) Wrap(value, content string) string {
	content = f.closing.ReplaceAllStringFunc(content, html.EscapeString)
	return fmt.Sprintf("<%s %s=\"%s\">\n%s\n</%s>", f.tag, f.attr, html.EscapeString(value), content, f.tag)
}
