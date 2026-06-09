// Package glob provides simplified glob matching supporting *, **, and ?.
// It is a shared engine package — usable by filelist (gitignore), agent (glob tool),
// and any other package that needs path-pattern matching without external dependencies.
package glob

// Match reports whether the glob pattern matches the name.
// Supported wildcards:
//   - * matches any sequence of characters except /
//   - ** matches any sequence of characters including /
//   - ? matches any single character except /
func Match(pattern, name string) bool {
	return doGlob([]rune(pattern), []rune(name))
}

// doGlob is the recursive core of Match. It consumes rune slices left-to-right,
// dispatching to globDoubleStar or globSingleStar on wildcard tokens.
func doGlob(pattern, name []rune) bool {
	for len(pattern) > 0 {
		switch {
		case len(pattern) >= 2 && pattern[0] == '*' && pattern[1] == '*':
			return globDoubleStar(pattern, name)

		case pattern[0] == '*':
			return globSingleStar(pattern[1:], name)

		case pattern[0] == '?':
			if len(name) == 0 || name[0] == '/' {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]

		default:
			if len(name) == 0 || pattern[0] != name[0] {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		}
	}
	return len(name) == 0
}

// globDoubleStar handles ** which matches any number of path segments.
// It only tries suffixes at path-segment boundaries (start of string or after /).
func globDoubleStar(pattern, name []rune) bool {
	rest := pattern[2:]
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return true
	}
	// Try matching rest at segment boundaries only.
	for i := range len(name) + 1 {
		if i == 0 || (i > 0 && name[i-1] == '/') {
			if doGlob(rest, name[i:]) {
				return true
			}
		}
	}
	return false
}

// globSingleStar handles * which matches anything except /.
func globSingleStar(rest, name []rune) bool {
	limit := 0
	for limit < len(name) && name[limit] != '/' {
		limit++
	}
	for i := limit; i >= 0; i-- {
		if doGlob(rest, name[i:]) {
			return true
		}
	}
	return false
}
