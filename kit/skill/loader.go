package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/nib/kit/frontmatter"
)

// skillFile is the conventional filename inside each skill directory.
const skillFile = "SKILL.md"

// meta is the YAML frontmatter schema for a SKILL.md file. All fields
// are optional at the YAML level; a missing name falls back to the
// directory basename.
type meta struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed-tools"`
}

// validNameChars reports whether r is allowed in a skill name. Skill
// names become part of an LLM-facing tool name ("skill_<name>"), so the
// set matches the tool-name-safe alphabet.
func validNameChars(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Load parses every <root>/<name>/SKILL.md and returns the resulting
// skills. root is typically "<projectRoot>/.project/skills".
//
// Error policy mirrors the markdown command loader:
//   - Missing root → (nil, nil). "No skills here" is not a failure.
//   - Per-skill errors (unreadable file, malformed frontmatter, invalid
//     name) are accumulated via [errors.Join] and returned alongside
//     the skills that did parse, so the caller chooses strict-or-lenient
//     policy at the composition site.
//
// Only immediate subdirectories are scanned, each expected to contain a
// SKILL.md. A subdirectory without one is skipped silently (it may hold
// unrelated resources). Files directly under root are ignored.
func Load(root string) ([]Skill, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir %s: %w", root, err)
	}

	var skills []Skill
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), skillFile)
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		s, perr := parse(path, e.Name())
		if perr != nil {
			errs = append(errs, perr)
			continue
		}
		skills = append(skills, s)
	}
	return skills, errors.Join(errs...)
}

// parse reads a single SKILL.md and resolves it into a Skill. dirName is
// the containing directory's basename, used as the name fallback.
func parse(path, dirName string) (Skill, error) {
	// gosec G304: path is built from os.ReadDir entries under a
	// startup-configured root in the standard wiring, so attacker
	// traversal is not reachable. External callers passing untrusted
	// roots own that responsibility.
	data, err := os.ReadFile(path) //nolint:gosec // see comment above
	if err != nil {
		return Skill{}, fmt.Errorf("read %s: %w", path, err)
	}
	var m meta
	body, err := frontmatter.Split(data, &m)
	if err != nil {
		return Skill{}, fmt.Errorf("parse %s: %w", path, err)
	}

	name := strings.ToLower(strings.TrimSpace(m.Name))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(dirName))
	}
	if !validNameChars(name) {
		return Skill{}, fmt.Errorf("parse %s: invalid skill name %q (must match [a-z0-9_-]+)", path, name)
	}

	desc := strings.TrimSpace(m.Description)
	if desc == "" {
		return Skill{}, fmt.Errorf("parse %s: skill %q has no description (the model needs one to select it)", path, name)
	}

	return Skill{
		Name:         name,
		Description:  desc,
		Body:         strings.TrimSpace(body),
		AllowedTools: m.AllowedTools,
		Path:         path,
	}, nil
}
