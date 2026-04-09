// Package styleconfig loads and merges coding style configuration from
// global and project-level config files, with environment variable overrides.
// It determines which architectural style rules (SOLID, hexagonal, DDD, etc.)
// the agent should follow when generating code.
package styleconfig

import "slices"

// Config represents the on-disk shape of a style configuration file.
// Both global and project files share this format. Fields left unset
// are inherited from lower-priority sources during merge.
type Config struct {
	Styles map[string]Style `json:"styles,omitempty"`
	Active string           `json:"active,omitempty"`
}

// Style defines a coding style with enforceable rules and optional lint commands.
type Style struct {
	// Name is the human-readable display name (e.g. "SOLID + Hexagonal").
	Name string `json:"name"`
	// Description summarizes when to use this style.
	Description string `json:"description,omitempty"`
	// Rules lists the enforceable principles for this style.
	Rules []Rule `json:"rules"`
	// LintCmd lists shell commands to run after edits for style validation.
	// The placeholder {file} is replaced with the edited file's relative path,
	// and {dir} is replaced with the file's directory (for package-level linting).
	LintCmd []string `json:"lint_cmd,omitempty"`
}

// Rule is a single enforceable principle within a coding style.
type Rule struct {
	// Name is a short label for the rule (e.g. "Single Responsibility").
	Name string `json:"name"`
	// Instruction is the directive the agent must follow.
	Instruction string `json:"instruction"`
	// Enforcement is "hard" (structural, verifiable — violations are rejected)
	// or "soft" (judgment-based — violations are flagged but not blocked).
	Enforcement string `json:"enforcement"`
}

// Resolved holds the final merged style ready for use by the agent.
// Nil when no style is active.
type Resolved struct {
	// Name is the active style's display name.
	Name string
	// Rules are the flattened enforceable principles.
	Rules []Rule
	// LintCmd lists shell commands for post-edit style validation.
	LintCmd []string
}

// StyleNames returns the sorted list of style names in the config.
// Returns nil if no styles are defined.
func (c *Config) StyleNames() []string {
	if len(c.Styles) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Styles))
	for name := range c.Styles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
