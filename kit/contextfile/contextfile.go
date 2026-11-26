// Package contextfile loads repo-carried agent instruction files
// (AGENTS.md / CLAUDE.md) and renders them as a system-prompt section
// that marks them as reference data, not instructions.
package contextfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/latebit-io/nib/kit/internal/filecap"
)

// MaxBytes is the size cap for a context file. Instruction files are
// prose; anything approaching this bound is either generated output or
// hostile, and is rejected rather than truncated — cutting an
// instruction file mid-sentence could drop the constraint that mattered.
const MaxBytes = 64 << 10 // 64 KiB

// candidates are the accepted file names in one directory, in
// preference order. First found wins, so a directory carrying both
// AGENTS.md and CLAUDE.md injects one document.
var candidates = []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// File is one loaded context file.
type File struct {
	// Path is the file's absolute path, used to label the injected
	// section so the model (and a transcript reader) can see where the
	// instructions came from.
	Path string
	// Content is the file body, verbatim.
	Content string
}

// MaxTotalBytes caps a chain's combined size, so a deep tree of
// instruction files cannot fill the context window.
const MaxTotalBytes = 2 * MaxBytes

// Load returns root's context file, if one exists: [LoadChain] for a
// working directory that is the root.
func Load(root string) ([]File, error) {
	return LoadChain(root, root)
}

// LoadChain returns the context files from root down to dir, nearest
// last: AGENTS.md gives the nearest file precedence. A file that cannot
// be used is reported in err, and the usable ones are still returned.
func LoadChain(root, dir string) ([]File, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("contextfile: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("contextfile: %w", err)
	}
	// A dir outside root contributes nothing: only root's file loads.
	rel, err := filepath.Rel(root, dir)
	if err != nil || !filepath.IsLocal(rel) {
		rel = "."
	}
	// Reads go through the root, so a file that is a symlink to
	// somewhere outside the project cannot reach the prompt.
	confined, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("contextfile: %w", err)
	}
	defer func() { _ = confined.Close() }() // read-only handle; nothing to flush

	var files []File
	var errs []error
	total := 0
	// Walking up from dir visits the nearest first, so the total cap
	// drops the farthest files. A relative path always ends at ".".
	for cur := rel; ; cur = filepath.Dir(cur) {
		f, ok, err := loadDir(confined, root, cur)
		switch {
		case err != nil:
			errs = append(errs, err)
		case ok && total+len(f.Content) > MaxTotalBytes:
			errs = append(errs, fmt.Errorf("contextfile: %s skipped: the chain exceeds the %d-byte total cap", f.Path, MaxTotalBytes))
		case ok:
			total += len(f.Content)
			files = append(files, f)
		}
		if cur == "." {
			break
		}
	}
	slices.Reverse(files)
	return files, errors.Join(errs...)
}

// loadDir reads the context file of dir, a path under root: the first
// candidate that exists. ok is false when the directory carries none.
func loadDir(confined *os.Root, root, dir string) (f File, ok bool, err error) {
	for _, name := range candidates {
		rel := filepath.Join(dir, name)
		body, err := filecap.ReadRoot(confined, rel, MaxBytes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return File{}, false, fmt.Errorf("contextfile: %w", err)
		}
		return File{Path: filepath.Join(root, rel), Content: string(body)}, true, nil
	}
	return File{}, false, nil
}
