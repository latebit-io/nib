package wire

import (
	"github.com/latebit-io/nib/coding/prompts"
	"github.com/latebit-io/nib/kit/contextfile"
)

// LoadContextFiles loads the project root's repo-carried instruction
// file (AGENTS.md / CLAUDE.md, first found) and converts it to the
// prompt-injection shape. Returns (nil, nil) when the project carries
// none — the common case. An unreadable or oversize file returns an
// error for the caller to log; startup proceeds without injection.
func LoadContextFiles(root string) ([]prompts.ContextFile, error) {
	files, err := contextfile.Load(root)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil // keep the documented (nil, nil) no-file contract; make() would yield a non-nil empty slice.
	}
	out := make([]prompts.ContextFile, 0, len(files))
	for _, f := range files {
		out = append(out, prompts.ContextFile{Path: f.Path, Content: f.Content})
	}
	return out, nil
}
