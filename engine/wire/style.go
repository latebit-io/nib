package wire

import (
	"log/slog"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/lint"
	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/llmconfig"
	"github.com/latebit-io/junto/engine/styleconfig"
)

// StyleResult holds the resolved style configuration and the effective
// linters to run during post-task review.
type StyleResult struct {
	// Config is the merged style configuration (all available styles).
	// Always non-nil — builtins are loaded even when no config files exist.
	Config *styleconfig.Config
	// Resolved is the active style after merge. Nil when no style is active.
	Resolved *styleconfig.Resolved
	// AgentStyle is the prompt-ready data for the agent. Nil when no style is active.
	AgentStyle *agent.CodingStyleData
	// Linters is the effective set of linters for the active style. It is
	// either the style's configured lint_cmd wrapped as raw adapters, or
	// DefaultLinters when the style has no explicit lint_cmd. Nil when no
	// style is active or no linter is available.
	Linters []lint.Linter
	// DefaultLinters is the auto-detected linter set for the project language.
	// Used as fallback when a style has no explicit lint_cmd (e.g. during
	// runtime style cycling).
	DefaultLinters []lint.Linter
}

// NewStyle resolves the coding style configuration and converts it to
// agent-ready prompt data. Returns a zero StyleResult when no style is active.
func NewStyle(projectRoot string) StyleResult {
	cfg, resolved := styleconfig.Resolve(projectRoot)

	// Auto-detect project-appropriate linters. Stored on the result so
	// callers can reuse them during runtime style cycling.
	defaults := lint.Detect(projectRoot)

	if resolved == nil {
		slog.Debug("wire: no active coding style")
		return StyleResult{Config: cfg, DefaultLinters: defaults}
	}

	slog.Info("wire: coding style active", "style", resolved.Name)

	return StyleResult{
		Config:         cfg,
		Resolved:       resolved,
		AgentStyle:     agent.NewCodingStyleData(resolved.Name, ConvertRules(resolved.Rules)),
		Linters:        LintersForStyle(resolved.LintCmd, defaults),
		DefaultLinters: defaults,
	}
}

// LintersForStyle returns the effective linters for a style: if lint_cmd is
// set, each command is wrapped in a raw adapter; otherwise defaults are
// returned. Used both at startup and during runtime style cycling.
func LintersForStyle(lintCmd []string, defaults []lint.Linter) []lint.Linter {
	if len(lintCmd) > 0 {
		return lint.FromShellCommands(lintCmd)
	}
	return defaults
}

// NewStyleEvaluator creates a StyleEvaluator from the resolved style config
// and an LLM provider. If the style's EvaluatorModel is set, a new provider
// is created for that model using the given LLM config. Returns nil when the
// evaluator is disabled in config or no provider is available.
// Use [ForceStyleEvaluator] when the developer explicitly toggles the evaluator on.
func NewStyleEvaluator(resolved *styleconfig.Resolved, mainProvider llm.Provider, llmCfg *llmconfig.Config) *agent.StyleEvaluator {
	if resolved == nil || !resolved.Evaluator {
		return nil
	}
	return ForceStyleEvaluator(resolved, mainProvider, llmCfg)
}

// ForceStyleEvaluator creates a StyleEvaluator regardless of the config's
// Evaluator flag. Used when the developer explicitly enables the evaluator
// at runtime via Alt+V. Honors EvaluatorModel if configured.
// Returns nil when no provider is available.
func ForceStyleEvaluator(resolved *styleconfig.Resolved, mainProvider llm.Provider, llmCfg *llmconfig.Config) *agent.StyleEvaluator {
	if resolved == nil {
		return nil
	}

	provider := mainProvider
	if resolved.EvaluatorModel != "" && llmCfg != nil {
		// Try to create a provider for the evaluator model using the active profile.
		if rp := llmconfig.ResolveProfile(llmCfg, llmCfg.Active); rp != nil {
			rp.Model = resolved.EvaluatorModel
			if p := rp.NewProvider(); p != nil {
				provider = p
				slog.Info("wire: style evaluator using dedicated model", "model", resolved.EvaluatorModel)
			}
		}
	}

	if provider == nil {
		slog.Warn("wire: style evaluator enabled but no provider available")
		return nil
	}

	data := agent.NewCodingStyleData(resolved.Name, ConvertRules(resolved.Rules))

	slog.Info("wire: style evaluator enabled", "style", resolved.Name, "rules", len(data.Rules))
	return agent.NewStyleEvaluator(provider, data.Rules, 0) // 0 = default timeout
}

// ConvertRules translates styleconfig rules into agent-ready StyleRule values.
// Used by NewStyle and by the TUI for runtime style switching.
func ConvertRules(rules []styleconfig.Rule) []agent.StyleRule {
	out := make([]agent.StyleRule, len(rules))
	for i, r := range rules {
		out[i] = agent.StyleRule{Name: r.Name, Instruction: r.Instruction, Enforcement: r.Enforcement}
	}
	return out
}
