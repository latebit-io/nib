package filelist

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// matcher checks paths against a set of gitignore patterns.
// Patterns are evaluated in order; the last matching pattern wins.
type matcher struct {
	patterns []pattern
	// baseDir is the directory containing the .gitignore, relative to the
	// project root (empty string for root-level). Anchored patterns are
	// matched against paths relative to this directory.
	baseDir string
}

type pattern struct {
	// The glob pattern (cleaned, no leading/trailing whitespace).
	glob string
	// Negated is true for ! patterns (re-include).
	negated bool
	// DirOnly is true for patterns ending in / (match directories only).
	dirOnly bool
	// Anchored is true for patterns containing / (relative to gitignore location).
	anchored bool
}

// loadGitignore reads a .gitignore file and returns a matcher.
// relDir is the directory containing the .gitignore relative to the project
// root (empty string for root-level). Returns nil if the file doesn't exist
// or is empty.
func loadGitignore(path, relDir string) *matcher {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to open .gitignore", "path", path, "err", err)
		}
		return nil
	}
	defer func() { _ = f.Close() }()

	var patterns []pattern
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		p, ok := parseLine(line)
		if ok {
			patterns = append(patterns, p)
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("failed to read .gitignore", "path", path, "err", err)
		return nil
	}
	if len(patterns) == 0 {
		return nil
	}
	return &matcher{patterns: patterns, baseDir: relDir}
}

// parseLine parses a single gitignore line into a pattern.
// Returns false for blank lines and comments.
func parseLine(line string) (pattern, bool) {
	// Trim trailing whitespace (but not leading — significant for patterns).
	line = strings.TrimRight(line, " \t")

	// Skip blank lines and comments.
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}

	p := pattern{}

	// Negation: leading !
	if strings.HasPrefix(line, "!") {
		p.negated = true
		line = line[1:]
	}

	// Remove leading backslash (escapes # or !).
	line = strings.TrimPrefix(line, "\\")

	// Directory-only: trailing /
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimRight(line, "/")
	}

	// Anchored: a leading / or a / in the pattern (except in **/).
	// Patterns like **/*.tmp are not anchored — they match at any depth.
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = line[1:]
	} else {
		// Check for / that isn't part of **/ — those anchor the pattern.
		stripped := strings.ReplaceAll(line, "**/", "")
		if strings.Contains(stripped, "/") {
			p.anchored = true
		}
	}

	p.glob = line
	return p, p.glob != ""
}

// match checks if a relative path matches any pattern.
// Returns (matched, negated). If no pattern matches, returns (false, false).
// relPath should use forward slashes. isDir indicates if the path is a directory.
func (m *matcher) match(relPath string, isDir bool) (matched bool, negated bool) {
	if m == nil {
		return false, false
	}

	// For anchored patterns, match against the path relative to this
	// .gitignore's directory, not the project root.
	localPath := relPath
	if m.baseDir != "" && strings.HasPrefix(relPath, m.baseDir+"/") {
		localPath = relPath[len(m.baseDir)+1:]
	}

	// Evaluate patterns in order — last match wins.
	result := false
	neg := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		target := relPath
		if p.anchored {
			target = localPath
		}
		if matchPattern(p.glob, target, p.anchored) {
			result = true
			neg = p.negated
		}
	}
	return result, neg
}

// matchPattern checks if a glob pattern matches a path.
// If anchored, the pattern must match the full relative path.
// If unanchored, the pattern can match just the basename or any suffix.
func matchPattern(glob, relPath string, anchored bool) bool {
	if anchored {
		return globMatch(glob, relPath)
	}
	// Unanchored: try basename first, then full path.
	base := filepath.Base(relPath)
	if globMatch(glob, base) {
		return true
	}
	return globMatch(glob, relPath)
}

// globMatch implements simplified glob matching supporting *, **, and ?.
func globMatch(pattern, name string) bool {
	return doGlob([]rune(pattern), []rune(name))
}

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
func globDoubleStar(pattern, name []rune) bool {
	rest := pattern[2:]
	if len(rest) > 0 && rest[0] == '/' {
		rest = rest[1:]
	}
	// Try matching rest against every suffix of name.
	for i := range len(name) + 1 {
		if doGlob(rest, name[i:]) {
			return true
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
