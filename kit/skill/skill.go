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
// v1 supports pure-prompt skills only. A skill that requests shell
// execution ([Skill.NeedsShell]) is parsed but refused at adapt time,
// because executing third-party shell needs the per-command approval
// surface that does not exist yet. The refusal is the seam the future
// script-skill extension slots into — see [Discover].
package skill

import "strings"

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
	// Path is the source SKILL.md path, kept for diagnostics.
	Path string
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
// allowed-tools list. Such skills are refused in v1 (no bash-approval
// surface yet); the predicate is the single branch the future
// script-skill adapter flips. Matching is whole-token, not substring,
// so legitimate tool names like "publish" or "run_tests" are not
// misclassified.
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
