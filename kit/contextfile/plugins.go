package contextfile

import (
	"path/filepath"

	"github.com/latebit-io/nib/kit"
)

// Plugins lists files for a `--plugins` manifest, so every binary
// describes its injected instruction files the same way.
func Plugins(files []File) []kit.Plugin {
	out := make([]kit.Plugin, 0, len(files))
	for _, f := range files {
		out = append(out, kit.Plugin{
			Kind:        kit.KindContext,
			Name:        filepath.Base(f.Path),
			Description: "project instructions, injected into the system prompt",
			Source:      f.Path,
		})
	}
	return out
}
