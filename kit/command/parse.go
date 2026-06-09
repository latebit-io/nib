package command

import "strings"

// parseSlash recognizes a slash command in input. Returns the
// canonical lowercase name, the raw argument string after the
// first run of spaces, and ok=true on a syntactic match.
//
// ok=false means "not a command" — the caller should forward the
// input unchanged. parseSlash never returns ok=true for malformed
// command-shaped input (e.g. "/foo!", "/", multi-line input
// starting with "/"); those pass through to the agent as if the
// user had typed regular text.
//
// Recognition rules:
//   - Must start with "/".
//   - Name characters are [a-zA-Z0-9_-]; the canonical name is
//     lower-cased before return.
//   - Multi-line input ("\n" or "\r" anywhere) is rejected — slash
//     commands are single-line by contract.
//   - Multiple spaces between name and args collapse to a single
//     boundary; args is the trim-left of the post-space remainder.
//     "/foo" → ("foo", "", true). "/foo  bar baz" → ("foo", "bar baz", true).
//     "/foo " → ("foo", "", true).
func parseSlash(input string) (name, args string, ok bool) {
	if len(input) < 2 || input[0] != '/' {
		return "", "", false
	}
	rest := input[1:]
	if strings.ContainsAny(rest, "\n\r") {
		return "", "", false
	}

	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		name = rest
	} else {
		name = rest[:sp]
		args = strings.TrimLeft(rest[sp:], " ")
	}

	if !ValidName(name) {
		return "", "", false
	}
	return strings.ToLower(name), args, true
}

// ValidName reports whether s satisfies the canonical command-name
// rule: [a-zA-Z0-9_-]+, non-empty. The rule is case-insensitive at
// the validator; callers that need a canonical form should lowercase
// before storing.
//
// Single-sourced for the framework: the parser uses it on user-typed
// input, [Registry.Register] uses it on registered Definition names
// and aliases, and the markdown loader uses it on filename- or
// frontmatter-derived names. Changing the character class here
// changes it everywhere — the only correct behavior, since a name
// the parser accepts must be a name the registry accepts and vice
// versa.
func ValidName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
