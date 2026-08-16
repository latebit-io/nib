// Package skill loads model-invoked instruction bundles (the
// Anthropic SKILL.md layout) and adapts each into an [agent.Tool].
//
// A skill is selected by the model from a one-line description and its
// body is pulled into context only when invoked — so a skill is, in
// nib's terms, a tool the model can call: Definition() carries the
// description (always advertised, cheap), Execute() returns the body
// (loaded on demand). This rides the existing tool port; it is a
// bundled plug-in, not a core change.
//
// Shell-bearing skills ([Skill.NeedsShell]) load alongside prompt-only
// ones. The shell they request is not trusted on their say-so: the
// hosting agent rebinds each skill tool's directive runner onto its own
// bash gate ([kit.ShellBinder]), so per-command approval, the
// always-allow list, and subagent grants apply to a skill's shell
// exactly as to a model-issued bash call. Plugin-imported shell skills
// additionally require the plugin to be trusted before they load — see
// [DiscoverWithPlugins].
package skill

import (
	"strings"

	"github.com/latebit-io/nib/kit/toolperm"
)

// Source identifies which layer a skill was loaded from. Mirrors
// command.SourceKind: project-local skills shadow user-global ones of
// the same name (see [Merge]).
type Source string

const (
	// SourceProject is a skill under <projectRoot>/.project/skills.
	SourceProject Source = "project"
	// SourceGlobal is a skill under the user-global skills directory.
	SourceGlobal Source = "global"
	// SourcePlugin is a skill imported from a managed plugin's converted
	// tree. Lowest precedence of the three: a user's own project or
	// global skill of the same name shadows a third-party plugin skill.
	SourcePlugin Source = "plugin"
)

// Skill is a parsed SKILL.md: the model-facing metadata plus the
// instruction body injected on invocation. The zero value is not a
// valid skill — construct via [Load].
type Skill struct {
	// Name is the skill identifier (frontmatter `name`, else the
	// containing directory's basename), lowercased. The advertised
	// tool name is "skill_" + Name.
	Name string
	// Description is the one-line summary the model matches against to
	// decide whether to invoke the skill. Always advertised.
	Description string
	// Body is the instruction text injected as the tool result when the
	// model invokes the skill.
	Body string
	// AllowedTools is the frontmatter `allowed-tools` list. A non-empty
	// entry naming shell execution marks the skill script-bearing; see
	// [Skill.NeedsShell].
	AllowedTools []string
	// DisallowedTools is the frontmatter `disallowed-tools` list — tool
	// grants explicitly denied even if otherwise allowed. Deny wins; see
	// [Skill.Permissions].
	DisallowedTools []string
	// Path is the source SKILL.md path, kept for diagnostics.
	Path string
	// Source is the layer this skill was loaded from. Stamped by [Load].
	Source Source
	// PluginID is the managed-plugin id this skill was imported from,
	// empty for project/global skills. It is the provenance the trust
	// gate keys on: a [SourcePlugin] shell skill loads only if its plugin
	// is trusted. Stamped by [DiscoverWithPlugins].
	PluginID string
	// Context selects how the skill runs. Empty injects the body as
	// prompt instructions (the default). "fork" runs the body as an
	// isolated child agent instead — see [Skill.IsFork]. A forking skill
	// is not adapted to a prompt tool here; the coding layer turns it into
	// a spawn tool.
	Context string
	// Agent optionally names a subagent type to fork into when
	// Context=="fork". Empty forks an agent built from the skill itself.
	Agent string
}

// IsFork reports whether the skill runs as an isolated child agent
// (frontmatter `context: fork`) rather than injecting its body as prompt
// text.
func (s Skill) IsFork() bool { return strings.EqualFold(strings.TrimSpace(s.Context), "fork") }

// Merge deduplicates skills by name across precedence layers. Layers
// are passed highest-precedence first (e.g. Merge(project, global)), so
// the first occurrence of a name wins and any later same-name skill is
// returned in shadowed rather than winners. Within a single layer the
// first occurrence wins (a duplicate name inside one directory is
// itself shadowed) — the loader does not otherwise dedup, so this is
// the one place name collisions are resolved before adaptation.
func Merge(layers ...[]Skill) (winners, shadowed []Skill) {
	seen := make(map[string]bool)
	for _, layer := range layers {
		for _, s := range layer {
			if seen[s.Name] {
				shadowed = append(shadowed, s)
				continue
			}
			seen[s.Name] = true
			winners = append(winners, s)
		}
	}
	return winners, shadowed
}

// shellTools are the whole-word tokens in an allowed-tools entry that
// mark a skill as requesting shell execution. Matched against each
// token of an entry (split on non-alphanumerics), never as substrings:
// substring matching on short tokens like "sh" would falsely flag
// "publish"/"push"/"refresh", and "run" would flag "run_tests"/"rerun".
var shellTools = map[string]bool{
	"bash":    true,
	"sh":      true,
	"shell":   true,
	"cmd":     true,
	"exec":    true,
	"execute": true,
}

// tokenize lowercases s and splits it into alphanumeric runs, so
// "execute_command" → ["execute", "command"] and "Bash" → ["bash"].
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

// NeedsShell reports whether the skill requests shell execution via its
// allowed-tools list. Plugin-imported shell skills load only when their
// plugin is trusted ([DiscoverWithPlugins]); project and global ones
// load and rely on the hosting agent's bash gate. Matching is
// whole-token, not substring, so legitimate tool names like "publish"
// or "run_tests" are not misclassified.
func (s Skill) NeedsShell() bool {
	for _, t := range s.AllowedTools {
		for _, tok := range tokenize(t) {
			if shellTools[tok] {
				return true
			}
		}
	}
	return false
}

// Permissions compiles the skill's allowed/disallowed tool grants into a
// [toolperm.Matcher]. It fails CLOSED: if either grant field is
// malformed, the matcher permits nothing ([toolperm.DenyAll]) rather than
// dropping the bad rule — silently discarding a malformed disallowed-tools
// entry could let a broad allowed-tools rule through. A skill with no
// allowed-tools likewise yields a matcher that permits nothing.
func (s Skill) Permissions() *toolperm.Matcher {
	allow, aerr := toolperm.ParseField(s.AllowedTools)
	deny, derr := toolperm.ParseField(s.DisallowedTools)
	if aerr != nil || derr != nil {
		return toolperm.DenyAll()
	}
	return toolperm.New(allow, deny)
}
