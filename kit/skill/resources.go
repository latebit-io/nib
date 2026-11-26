package skill

import (
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
)

// maxListedResources caps the bundled-file listing; past it the note
// says the listing is incomplete.
const maxListedResources = 50

// ResourceNote returns the text to append to the skill's instructions
// on activation: the skill directory and the files bundled beside
// SKILL.md, listed but not read. Empty when the skill bundles nothing.
func (s Skill) ResourceNote() string {
	if s.Path == "" {
		return ""
	}
	dir := filepath.Dir(s.Path)
	// Abs fails only when the working directory is gone; the relative
	// form is still the right label then.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	files, more, err := bundled(dir)
	if err != nil {
		slog.Warn("skill: bundled-file listing is incomplete", "skill", s.Name, "dir", dir, "err", err)
	}
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nSkill directory: %s\n", dir)
	b.WriteString("Relative paths in this skill are relative to the skill directory.\n")
	b.WriteString("<skill_resources>\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  <file>%s</file>\n", f)
	}
	if more || err != nil {
		b.WriteString("  (listing incomplete: the directory holds more files)\n")
	}
	b.WriteString("</skill_resources>")
	return b.String()
}

// bundled lists the files under dir other than SKILL.md, slash-separated
// and relative to dir, in lexical order. more reports that the cap cut
// the listing short.
func bundled(dir string) (files []string, more bool, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Dot directories (.git) are tooling state, not skill content.
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if rel == skillFile {
			return nil
		}
		if len(files) == maxListedResources {
			more = true
			return filepath.SkipAll
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, more, err
}
