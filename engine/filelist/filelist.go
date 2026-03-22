// Package filelist provides project file listing with gitignore support.
// It is an engine-level package — no UI dependencies. Usable by any
// frontend (command palette, file finder) and by the agent (file search).
package filelist

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxFiles = 50000

// Walk returns all regular files under root, respecting .gitignore rules.
// Paths are relative to root with forward slashes. The .git directory is
// always excluded. Returns a sorted slice. Caps at 50k files with a warning.
func Walk(root string) ([]string, error) {
	root = filepath.Clean(root)
	var files []string
	err := walk(root, "", nil, &files)
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// walk recursively lists files under dir. relDir is the path relative to root.
// matchers is the stack of gitignore matchers from parent directories.
func walk(absDir, relDir string, matchers []*matcher, files *[]string) error {
	if len(*files) >= maxFiles {
		slog.Warn("file listing capped", "max", maxFiles)
		return nil
	}

	// Load .gitignore for this directory (if present).
	gi := loadGitignore(filepath.Join(absDir, ".gitignore"))
	if gi != nil {
		matchers = append(matchers, gi)
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()

		// Always skip .git directory.
		if name == ".git" && entry.IsDir() {
			continue
		}

		rel := name
		if relDir != "" {
			rel = relDir + "/" + name
		}

		isDir := entry.IsDir()

		// Check gitignore matchers (innermost wins, last match in each matcher wins).
		if ignored(matchers, rel, isDir) {
			continue
		}

		if isDir {
			if err := walk(filepath.Join(absDir, name), rel, matchers, files); err != nil {
				return err
			}
		} else {
			*files = append(*files, rel)
			if len(*files) >= maxFiles {
				slog.Warn("file listing capped", "max", maxFiles)
				return nil
			}
		}
	}

	return nil
}

// ignored checks the matcher stack for a path. Returns true if the path
// should be excluded. Evaluates matchers from innermost to outermost;
// the last matching pattern across all matchers determines the result.
func ignored(matchers []*matcher, relPath string, isDir bool) bool {
	result := false
	for _, m := range matchers {
		matched, negated := m.match(relPath, isDir)
		if matched {
			result = !negated
		}
	}
	return result
}

// IsHidden returns true if the path starts with a dot.
func IsHidden(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, ".")
}
