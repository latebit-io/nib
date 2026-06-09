// Package filelist provides project file listing with gitignore support.
// It is an engine-level package — no UI dependencies. Usable by any
// frontend (command palette, file finder) and by the agent (file search).
package filelist

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxFiles = 50000

// errFileCap is an internal sentinel used to stop traversal.
var errFileCap = errors.New("file listing capped")

// ErrCapped is returned by Walk when the file count exceeds the cap.
// The returned file list is partial but usable.
var ErrCapped = errors.New("file listing capped at 50000 files — results may be incomplete")

// Walk returns all regular files under root, respecting .gitignore rules.
// Paths are relative to root with forward slashes. The .git directory is
// always excluded. Returns a sorted slice. Caps at 50k files with a warning.
func Walk(root string) ([]string, error) {
	files, _, err := WalkWithDirs(root)
	return files, err
}

// WalkWithDirs returns all regular files and all non-ignored directory paths
// under root. Directories are included whether or not they contain files,
// so empty directories appear in the tree. Both slices are sorted.
func WalkWithDirs(root string) (files []string, dirs []string, err error) {
	root = filepath.Clean(root)
	walkErr := walk(root, "", nil, &files, &dirs)
	if walkErr == errFileCap {
		slog.Warn("file listing capped", "max", maxFiles)
		sort.Strings(files)
		sort.Strings(dirs)
		return files, dirs, ErrCapped
	} else if walkErr != nil {
		return nil, nil, walkErr
	}
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs, nil
}

// walk recursively lists files under dir. relDir is the path relative to root.
// matchers is the stack of gitignore matchers from parent directories.
func walk(absDir, relDir string, matchers []*matcher, files, dirs *[]string) error {
	if len(*files) >= maxFiles {
		return errFileCap
	}

	// Load .gitignore for this directory (if present).
	gi := loadGitignore(filepath.Join(absDir, ".gitignore"), relDir)
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
			*dirs = append(*dirs, rel)
			if err := walk(filepath.Join(absDir, name), rel, matchers, files, dirs); err != nil {
				return err
			}
		} else if entry.Type().IsRegular() {
			*files = append(*files, rel)
			if len(*files) >= maxFiles {
				return errFileCap
			}
		}
	}

	return nil
}

// ignored checks the matcher stack for a path. Returns true if the path
// should be excluded. Evaluates matchers from outermost to innermost;
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
