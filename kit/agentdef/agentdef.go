// Package agentdef loads subagent definitions — the Claude Code
// `agents/<name>.md` layout — into a validated [Definition]. A definition
// is the static spec a future spawn step instantiates into a child agent:
// a system prompt (the markdown body) plus the model, effort, turn cap,
// tool grants, preloaded skills, and isolation mode the child runs under.
//
// This package is pure: parsing, validation, and permission compilation,
// no agent construction or execution. It mirrors the skill and command
// loaders (shared frontmatter parser, fail-loud grant validation, a
// source layer stamped on each definition) so the three artifact kinds
// behave consistently.
package agentdef

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

// agentFileExt is the extension of an agent definition file.
const agentFileExt = ".md"

// maxAgentFileBytes caps how much of a definition file is read — the body
// is a system prompt, never near this size; the cap bounds memory against
// a hostile or accidental giant file.
const maxAgentFileBytes = 1 << 20 // 1 MiB

// Source identifies which layer a definition was loaded from. Mirrors
// [skill.Source] / [command.SourceKind].
type Source string

const (
	// SourceProject is an agent under <projectRoot>/.project/agents.
	SourceProject Source = "project"
	// SourceGlobal is an agent under the user-global agents directory.
	SourceGlobal Source = "global"
	// SourcePlugin is an agent imported from a managed plugin.
	SourcePlugin Source = "plugin"
)

// Isolation is how a spawned child agent is sandboxed on the filesystem.
type Isolation string

const (
	// IsolationNone runs the child in the parent's working tree.
	IsolationNone Isolation = ""
	// IsolationWorktree runs the child in a dedicated git worktree.
	IsolationWorktree Isolation = "worktree"
)

// validEfforts is the set of accepted effort levels (empty = inherit).
var validEfforts = map[string]bool{
	"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// Definition is a parsed, validated subagent specification.
type Definition struct {
	// Name is the subagent identifier (frontmatter `name`, else the file
	// basename), lowercased — the type a spawn call selects.
	Name string
	// Description is the one-line summary used to choose the agent.
	Description string
	// SystemPrompt is the markdown body: the child's system prompt.
	SystemPrompt string
	// AllowedTools / DisallowedTools are the child's tool grants
	// (frontmatter `tools` / `disallowedTools`). Compile via [Permissions].
	AllowedTools    []string
	DisallowedTools []string
	// Model overrides the child's model (empty = inherit the parent's).
	Model string
	// Effort overrides the child's reasoning effort (empty = inherit).
	Effort string
	// MaxTurns caps the child's turns (0 = engine default).
	MaxTurns int
	// Skills are skill names to preload into the child's context.
	Skills []string
	// Isolation selects the child's filesystem sandbox.
	Isolation Isolation
	// Path is the source file path, kept for diagnostics.
	Path string
	// Source is the layer this definition was loaded from.
	Source Source
	// PluginID is the managed-plugin id for [SourcePlugin] definitions,
	// empty otherwise. Provenance for the trust gate.
	PluginID string
}

// Permissions compiles the definition's tool grants into a
// [toolperm.Matcher]. It fails closed on a malformed grant (parsing is
// also validated at load, so this is defense in depth).
func (d Definition) Permissions() *toolperm.Matcher {
	allow, aerr := toolperm.ParseField(d.AllowedTools)
	deny, derr := toolperm.ParseField(d.DisallowedTools)
	if aerr != nil || derr != nil {
		return toolperm.DenyAll()
	}
	return toolperm.New(allow, deny)
}

// meta is the YAML frontmatter schema for an agent definition.
type meta struct {
	Name            string                 `yaml:"name"`
	Description     string                 `yaml:"description"`
	Tools           frontmatter.StringList `yaml:"tools"`
	DisallowedTools frontmatter.StringList `yaml:"disallowedTools"`
	Model           string                 `yaml:"model"`
	Effort          string                 `yaml:"effort"`
	MaxTurns        int                    `yaml:"maxTurns"`
	Skills          frontmatter.StringList `yaml:"skills"`
	Isolation       string                 `yaml:"isolation"`
}

// validNameChars reports whether name is in the type-safe alphabet (the
// name becomes a spawn selector, like a tool name).
func validNameChars(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// Parse reads and validates a single agent definition file. dirName-free:
// the name falls back to the file's basename. source stamps the layer.
func Parse(path string, source Source) (Definition, error) {
	data, err := filecap.Read(path, maxAgentFileBytes, "agent")
	if err != nil {
		return Definition{}, fmt.Errorf("read %s: %w", path, err)
	}
	var m meta
	body, err := frontmatter.Split(data, &m)
	if err != nil {
		return Definition{}, fmt.Errorf("parse %s: %w", path, err)
	}

	name := strings.ToLower(strings.TrimSpace(m.Name))
	if name == "" {
		base := filepath.Base(path)
		name = strings.ToLower(strings.TrimSuffix(base, agentFileExt))
	}
	if !validNameChars(name) {
		return Definition{}, fmt.Errorf("parse %s: invalid agent name %q (must match [a-z0-9_-]+)", path, name)
	}

	desc := strings.TrimSpace(m.Description)
	if desc == "" {
		return Definition{}, fmt.Errorf("parse %s: agent %q has no description (needed to select it)", path, name)
	}
	prompt := strings.TrimSpace(body)
	if prompt == "" {
		return Definition{}, fmt.Errorf("parse %s: agent %q has an empty system prompt", path, name)
	}

	// Fail loud on malformed grants rather than silently dropping a deny.
	if _, err := toolperm.ParseField([]string(m.Tools)); err != nil {
		return Definition{}, fmt.Errorf("parse %s: invalid tools: %w", path, err)
	}
	if _, err := toolperm.ParseField([]string(m.DisallowedTools)); err != nil {
		return Definition{}, fmt.Errorf("parse %s: invalid disallowedTools: %w", path, err)
	}
	if !validEfforts[strings.ToLower(strings.TrimSpace(m.Effort))] {
		return Definition{}, fmt.Errorf("parse %s: invalid effort %q (low|medium|high|xhigh|max)", path, m.Effort)
	}
	if m.MaxTurns < 0 {
		return Definition{}, fmt.Errorf("parse %s: maxTurns %d must be >= 0", path, m.MaxTurns)
	}
	isolation := Isolation(strings.TrimSpace(m.Isolation))
	if isolation != IsolationNone && isolation != IsolationWorktree {
		return Definition{}, fmt.Errorf("parse %s: invalid isolation %q (worktree or empty)", path, m.Isolation)
	}

	return Definition{
		Name:            name,
		Description:     desc,
		SystemPrompt:    prompt,
		AllowedTools:    []string(m.Tools),
		DisallowedTools: []string(m.DisallowedTools),
		Model:           strings.TrimSpace(m.Model),
		Effort:          strings.ToLower(strings.TrimSpace(m.Effort)),
		MaxTurns:        m.MaxTurns,
		Skills:          []string(m.Skills),
		Isolation:       isolation,
		Path:            path,
		Source:          source,
	}, nil
}

// LoadDir parses every <root>/*.md agent definition and returns them,
// each stamped with source. Mirrors the skill/command loaders' policy:
//   - Missing root → (nil, nil): "no agents here" is not a failure.
//   - Per-file errors accumulate via [errors.Join] and are returned
//     alongside the definitions that did parse, so the caller chooses
//     strict-or-lenient handling.
//
// Only files directly under root are scanned (non-recursive).
func LoadDir(root string, source Source) ([]Definition, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read agents dir %s: %w", root, err)
	}

	var defs []Definition
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), agentFileExt) {
			continue
		}
		d, perr := Parse(filepath.Join(root, e.Name()), source)
		if perr != nil {
			errs = append(errs, perr)
			continue
		}
		defs = append(defs, d)
	}
	return defs, errors.Join(errs...)
}
