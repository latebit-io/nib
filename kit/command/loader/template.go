// Package loader builds [kit/command.PromptCommand] instances from
// on-disk markdown templates. Loaders construct PromptCommands; the
// type system prevents them from constructing HandlerCommands, so
// markdown files cannot mutate session state — that is the trust
// firewall the layered design relies on.
//
// The package is split into two concerns:
//
//   - template.go (this file) handles argument substitution against a
//     parsed template body.
//   - markdown.go discovers files, parses YAML frontmatter, and
//     constructs PromptCommand instances.
//
// Loaders depend only on kit/command and the standard library
// (plus gopkg.in/yaml.v3 for frontmatter). They must not import
// coding/, engine/, or tui/ — markdown templates are
// frontend-agnostic by construction.
package loader

import (
	"regexp"
	"strconv"
	"strings"
)

// substRe matches the three substitution forms supported in
// templates: $ARGUMENTS (raw, unsplit input), $@ (positional args
// rejoined with spaces), and $N (1-indexed positional). Order in
// the alternation does not matter for correctness — Go's regexp
// engine returns the leftmost match — but $ARGUMENTS is listed
// first to make the precedence intent obvious to readers.
//
// Notably absent: $0 (no command name in scope at Render time),
// $* (an alias for $@ in shells; the plan picked one form to keep
// the surface small), and any escape sequence for a literal $.
// Templates that need a literal "$1" cannot have one — accepted
// limitation; revisit if real templates demand it.
var substRe = regexp.MustCompile(`\$ARGUMENTS\b|\$@|\$(\d+)`)

// Substitute applies argument substitution to the template body
// and returns the rendered prompt.
//
// Substitution rules:
//
//   - $ARGUMENTS — replaced with args verbatim (preserving any
//     internal whitespace, including leading/trailing spaces).
//   - $@         — replaced with the whitespace-tokenized args
//     rejoined with single spaces. Equivalent to
//     $ARGUMENTS for normal inputs; differs only
//     when the user typed multiple consecutive
//     spaces and the template author wants the
//     normalized form.
//   - $N (N≥1)   — replaced with the Nth whitespace-tokenized
//     positional argument, or empty when N exceeds
//     the count.
//
// Tokenization uses [strings.Fields]: any run of unicode whitespace
// separates tokens, leading/trailing whitespace is stripped, and
// empty input yields zero positional args. Quoting and escapes are
// not honored — markdown commands are not shells. Templates that
// need to preserve a literal whitespace-bearing argument should
// use $ARGUMENTS and accept the input as-is.
//
// Substitution is single-pass: a substitution result that itself
// contains $-tokens is NOT re-expanded. This avoids quadratic
// blow-up on adversarial inputs and makes the rendered output
// predictable.
func Substitute(template, args string) string {
	positional := strings.Fields(args)
	return substRe.ReplaceAllStringFunc(template, func(match string) string {
		switch match {
		case "$ARGUMENTS":
			return args
		case "$@":
			return strings.Join(positional, " ")
		default:
			// $N — match[1:] is the digits. Errors are impossible
			// here because the regex only matches \d+; Atoi cannot
			// fail on a non-empty all-digit string.
			n, _ := strconv.Atoi(match[1:])
			if n >= 1 && n <= len(positional) {
				return positional[n-1]
			}
			return ""
		}
	})
}
