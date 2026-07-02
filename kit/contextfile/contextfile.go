// Package contextfile loads repo-local agent instruction files
// (AGENTS.md / CLAUDE.md) for injection into a consumer's system prompt.
// The file is developer-authored, repo-carried context — build commands,
// conventions, style rules — that an agent should honor on any project
// without a memory bootstrap.
//
// Trust: the loaded content is reference data from the repository, not
// an instruction channel. Consumers must inject it behind the same trust
// framing as any external input (never overrides system or developer
// instructions; commands found inside are not executed). The loader
// enforces only the mechanical half of that posture: a hard size cap.
package contextfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/latebit-io/nib/kit/internal/filecap"
)

// MaxBytes is the size cap for a context file. Instruction files are
// prose; anything approaching this bound is either generated output or
// hostile, and is rejected rather than truncated — cutting an
// instruction file mid-sentence could drop the constraint that mattered.
const MaxBytes = 64 << 10 // 64 KiB

// candidates are the accepted file names at the project root, in
// preference order. First found wins; the rest are ignored so a repo
// carrying both AGENTS.md and CLAUDE.md injects one coherent document.
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

// Load returns the project root's context file, if one exists. The
// result slice has zero or one element in v1 (the slice shape leaves
// room for an ancestor walk without a signature break). A candidate
// that exists but exceeds [MaxBytes] yields an error naming the cap;
// the caller decides whether to log-and-continue or fail.
func Load(root string) ([]File, error) {
	for _, name := range candidates {
		path := filepath.Join(root, name)
		body, err := filecap.Read(path, MaxBytes, "context file")
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("contextfile: %w", err)
		}
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			abs = path // fall back to the joined path; labeling only, never dereferenced.
		}
		return []File{{Path: abs, Content: string(body)}}, nil
	}
	return nil, nil
}
