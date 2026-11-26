package contextfile

import (
	"path/filepath"

	"github.com/latebit-io/nib/kit"
)

// Plugins lists files for a `--plugins` manifest, so every binary
// describes its injected instruction files the same way. Each entry is
// named by its path under root, which tells nested files apart.
func Plugins(root string, files []File) []kit.Plugin {
	out := make([]kit.Plugin, 0, len(files))
	for _, f := range files {
		name, err := filepath.Rel(root, f.Path)
		if err != nil || !filepath.IsLocal(name) {
			name = filepath.Base(f.Path) // not under root: the bare name still identifies it
		}
		out = append(out, kit.Plugin{
			Kind:        kit.KindContext,
			Name:        filepath.ToSlash(name),
			Description: "project instructions, injected into the system prompt",
			Source:      f.Path,
		})
	}
	return out
}
