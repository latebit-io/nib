package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/nib/kit/frontmatter"
	"github.com/latebit-io/nib/kit/internal/filecap"
	"github.com/latebit-io/nib/kit/toolperm"
)

// skillFile is the conventional filename inside each skill directory.
const skillFile = "SKILL.md"

// maxSkillFileBytes caps how much of a SKILL.md is read. A skill body is
// instruction text, never near this size; the cap bounds memory against
// a hostile or accidental giant file in an untrusted project's
// .project/skills (a local DoS otherwise).
const maxSkillFileBytes = 1 << 20 // 1 MiB

// meta is the YAML frontmatter schema for a SKILL.md file. All fields
// are optional at the YAML level; a missing name falls back to the
// directory basename.
type meta struct {
	Name            string                 `yaml:"name"`
	Description     string                 `yaml:"description"`
	AllowedTools    frontmatter.StringList `yaml:"allowed-tools"`
	DisallowedTools frontmatter.StringList `yaml:"disallowed-tools"`
	Context         string                 `yaml:"context"`
	Agent           string                 `yaml:"agent"`
}

// validNameChars reports whether name contains only characters allowed
// in a skill name. Skill names become part of an LLM-facing tool name
// ("skill_<name>"), so the set matches the tool-name-safe alphabet.
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
// skills, stamping each with source. root is typically
// "<projectRoot>/.project/skills" (with [SourceProject]) or the
// user-global skills directory (with [SourceGlobal]).
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
func Load(root string, source Source) ([]Skill, error) {
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
		s, perr := parse(path, e.Name(), source)
		if perr != nil {
			errs = append(errs, perr)
			continue
		}
		skills = append(skills, s)
	}
	return skills, errors.Join(errs...)
}

// parse reads a single SKILL.md and resolves it into a Skill. dirName is
// the containing directory's basename, used as the name fallback;
// source stamps the resulting skill's layer.
func parse(path, dirName string, source Source) (Skill, error) {
	// gosec G304: path is built from os.ReadDir entries under a
	// startup-configured root in the standard wiring, so attacker
	// traversal is not reachable (entry names are single path
	// components — no separators or ".."). External callers passing
	// untrusted roots own that responsibility. Size-capped so a hostile
	// or accidental giant SKILL.md cannot exhaust memory.
	data, err := filecap.Read(path, maxSkillFileBytes, "skill")
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

	// Validate tool-permission grants at load so a malformed
	// disallowed-tools rule fails loudly rather than silently dropping
	// when the matcher is built (which would fail closed but hide the bug).
	if _, err := toolperm.ParseField([]string(m.AllowedTools)); err != nil {
		return Skill{}, fmt.Errorf("parse %s: invalid allowed-tools: %w", path, err)
	}
	if _, err := toolperm.ParseField([]string(m.DisallowedTools)); err != nil {
		return Skill{}, fmt.Errorf("parse %s: invalid disallowed-tools: %w", path, err)
	}

	ctxMode := strings.ToLower(strings.TrimSpace(m.Context))
	if ctxMode != "" && ctxMode != "fork" {
		return Skill{}, fmt.Errorf("parse %s: invalid context %q (fork or empty)", path, m.Context)
	}
	// `agent:` selects a named subagent type to fork into. Resolving that
	// reference is not implemented yet, so accepting it would silently run
	// the wrong thing (a synthetic agent built from the skill) instead of
	// the named one. Reject it outright until the resolution path exists,
	// rather than dropping it on a non-fork skill or honoring it falsely on
	// a fork skill.
	if strings.TrimSpace(m.Agent) != "" {
		return Skill{}, fmt.Errorf("parse %s: skill %q sets agent:%q — agent references are not yet supported (omit it)", path, name, m.Agent)
	}

	return Skill{
		Name:            name,
		Description:     desc,
		Body:            strings.TrimSpace(body),
		AllowedTools:    []string(m.AllowedTools),
		DisallowedTools: []string(m.DisallowedTools),
		Context:         ctxMode,
		Agent:           strings.TrimSpace(m.Agent),
		Path:            path,
		Source:          source,
	}, nil
}
