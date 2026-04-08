package wire

import (
	"log/slog"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/styleconfig"
)

// StyleResult holds the resolved style configuration and an agent-ready
// CodingStyleData for prompt injection.
type StyleResult struct {
	// Config is the merged style configuration (all available styles).
	// Always non-nil — builtins are loaded even when no config files exist.
	Config *styleconfig.Config
	// Resolved is the active style after merge. Nil when no style is active.
	Resolved *styleconfig.Resolved
	// AgentStyle is the prompt-ready data for the agent. Nil when no style is active.
	AgentStyle *agent.CodingStyleData
}

// NewStyle resolves the coding style configuration and converts it to
// agent-ready prompt data. Returns a zero StyleResult when no style is active.
func NewStyle(projectRoot string) StyleResult {
	cfg, resolved := styleconfig.Resolve(projectRoot)
	if resolved == nil {
		slog.Debug("wire: no active coding style")
		return StyleResult{Config: cfg}
	}

	slog.Info("wire: coding style active", "style", resolved.Name)

	return StyleResult{
		Config:     cfg,
		Resolved:   resolved,
		AgentStyle: agent.NewCodingStyleData(resolved.Name, ConvertRules(resolved.Rules)),
	}
}

// ConvertRules translates styleconfig rules into agent-ready StyleRule values.
// Used by NewStyle and by the TUI for runtime style switching.
func ConvertRules(rules []styleconfig.Rule) []agent.StyleRule {
	out := make([]agent.StyleRule, len(rules))
	for i, r := range rules {
		out[i] = agent.StyleRule{Name: r.Name, Instruction: r.Instruction}
	}
	return out
}
