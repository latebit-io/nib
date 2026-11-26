package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/latebit-io/nib/kit/contextfile"
)

// loadContextFiles returns root's instruction files (AGENTS.md). A file
// that cannot be used is reported and skipped: the run goes on without it.
func loadContextFiles(root string) []contextfile.File {
	files, err := contextfile.Load(root)
	if err != nil {
		// slog is discarded without --debug, so say it on stderr too.
		fmt.Fprintf(os.Stderr, "warning: project instructions not loaded: %v\n", err)
		slog.Warn("context file not loaded", "root", root, "err", err)
	}
	return files
}

// withProjectInstructions appends the files' prompt section to base.
func withProjectInstructions(base string, files []contextfile.File) string {
	section := contextfile.Prompt(files)
	if section == "" {
		return base
	}
	return base + "\n\n" + section
}
