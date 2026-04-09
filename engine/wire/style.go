package wire

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

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

// detectLintCommands auto-detects appropriate lint commands based on the
// project language. Returns nil when no linter is detected.
func detectLintCommands(projectRoot string) []string {
	// Go project: check for go.mod and a linter on PATH.
	if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
		if _, err := exec.LookPath("golangci-lint"); err == nil {
			slog.Info("wire: auto-detected golangci-lint for Go project")
			return []string{"golangci-lint run {file}"}
		}
		// Fallback: go vet is always available in a Go project.
		if _, err := exec.LookPath("go"); err == nil {
			slog.Info("wire: auto-detected go vet for Go project (golangci-lint not found)")
			return []string{"go vet {file}"}
		}
	}

	return nil
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
