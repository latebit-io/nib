package wire

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/llmconfig"
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
	// DefaultLintCmd holds auto-detected lint commands for the project.
	// Used as fallback when a style has no explicit lint_cmd configured.
	DefaultLintCmd []string
}

// NewStyle resolves the coding style configuration and converts it to
// agent-ready prompt data. Returns a zero StyleResult when no style is active.
func NewStyle(projectRoot string) StyleResult {
	cfg, resolved := styleconfig.Resolve(projectRoot)

	// Auto-detect lint commands for the project language.
	// Stored on the result so callers can use it as fallback during cycling.
	defaultLint := detectLintCommands(projectRoot)

	if resolved == nil {
		slog.Debug("wire: no active coding style")
		return StyleResult{Config: cfg, DefaultLintCmd: defaultLint}
	}

	slog.Info("wire: coding style active", "style", resolved.Name)

	// Use auto-detected lint commands when none are explicitly configured.
	if len(resolved.LintCmd) == 0 {
		resolved.LintCmd = defaultLint
	}

	return StyleResult{
		Config:         cfg,
		Resolved:       resolved,
		AgentStyle:     agent.NewCodingStyleData(resolved.Name, ConvertRules(resolved.Rules)),
		DefaultLintCmd: defaultLint,
	}
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

// detectLintCommands auto-detects appropriate lint commands based on the
// project language. Returns nil when no linter is detected.
func detectLintCommands(projectRoot string) []string {
	// Go project: check for go.mod and a linter on PATH.
	// Use {dir} (package directory) instead of {file} — Go requires package-level
	// compilation, so linting a single file misses type definitions from sibling
	// files and produces false positives.
	if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
		if _, err := exec.LookPath("golangci-lint"); err == nil {
			slog.Info("wire: auto-detected golangci-lint for Go project")
			return []string{"golangci-lint run ./{dir}/..."}
		}
		// Fallback: go vet is always available in a Go project.
		if _, err := exec.LookPath("go"); err == nil {
			slog.Info("wire: auto-detected go vet for Go project (golangci-lint not found)")
			return []string{"go vet ./{dir}/..."}
		}
	}

	// Lua project: detected by .luacheckrc, main.lua (LÖVE convention), or any
	// .lua file at the root. Luacheck is the de facto standard linter; it
	// operates per-file cleanly, so {file} (not {dir}) is the right expansion.
	if isLuaProject(projectRoot) {
		if _, err := exec.LookPath("luacheck"); err == nil {
			slog.Info("wire: auto-detected luacheck for Lua project")
			return []string{"luacheck {file}"}
		}
		slog.Debug("wire: Lua project detected but luacheck not on PATH — no lint configured")
	}

	return nil
}

// isLuaProject reports whether projectRoot looks like a Lua project. The
// check is shallow (root-level only) — a deep walk would be wasted work
// for a signal the developer can override via explicit style config.
func isLuaProject(projectRoot string) bool {
	if _, err := os.Stat(filepath.Join(projectRoot, ".luacheckrc")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "main.lua")); err == nil {
		return true
	}
	matches, err := filepath.Glob(filepath.Join(projectRoot, "*.lua"))
	if err != nil {
		// filepath.Glob returns ErrBadPattern when the combined pattern is
		// malformed — possible if projectRoot contains unclosed brackets.
		// Log and fail closed so we don't silently misclassify the project.
		slog.Warn("wire: Lua project glob failed", "root", projectRoot, "err", err)
		return false
	}
	return len(matches) > 0
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
