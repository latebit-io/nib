package wire

import (
	"log/slog"
	"sync"

	"github.com/latebit-io/nib/coding/prompts"
	"github.com/latebit-io/nib/engine/lint"
	"github.com/latebit-io/nib/engine/styleconfig"
	"github.com/latebit-io/nib/engine/validate/architecture"
)

// PerFileLinterHolder is a thread-safe holder for the active style's
// per-file linters. The lintstage validator reads it; the TUI's
// CycleStyle closure updates it on style switch.
//
// Pattern parity with [styleconfig.ActiveProvider] — capturing a
// slice into a closure at pipeline construction would race when the
// TUI goroutine writes during a style cycle while the agent
// goroutine reads mid-Validate. The holder is the seam that lets
// runtime style cycling reach the validator without rebuilding the
// pipeline.
type PerFileLinterHolder struct {
	mu      sync.RWMutex
	linters []lint.Linter
}

// NewPerFileLinterHolder returns a holder seeded with the given
// initial linter set. Composition roots populate it from the
// resolved style at startup and on every style cycle. The slice is
// cloned defensively — see [PerFileLinterHolder.Set] for the
// rationale.
func NewPerFileLinterHolder(initial []lint.Linter) *PerFileLinterHolder {
	return &PerFileLinterHolder{linters: cloneLinters(initial)}
}

// Linters returns a snapshot of the current per-file linter set,
// defensively copied so a caller mutating the result cannot race
// with a concurrent Set. Today's only reader (lintstage) just
// iterates and never mutates, but the type's "thread-safe" contract
// has to hold against future callers without a hand-rolled
// "treat as immutable" convention. The underlying [lint.Linter]
// implementations are pointer-typed and shared — the cost we're
// guarding is the slice header / backing array, not the linter
// state itself.
func (h *PerFileLinterHolder) Linters() []lint.Linter {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return cloneLinters(h.linters)
}

// Set replaces the per-file linter set. The provided slice is
// cloned defensively so a caller that later mutates the slice
// (e.g. appends a new linter and re-Sets) cannot accidentally
// touch storage the holder is concurrently exposing through
// [PerFileLinterHolder.Linters]. Pass nil to disable per-file lint
// (e.g. when the developer cycles past the last style with `Alt+S`).
func (h *PerFileLinterHolder) Set(linters []lint.Linter) {
	h.mu.Lock()
	h.linters = cloneLinters(linters)
	h.mu.Unlock()
}

// cloneLinters returns a defensive copy of src so the holder's
// stored slice is decoupled from the caller's. Returns nil for
// empty input to preserve the "no per-file linter configured"
// sentinel that lintstage's Applicable check relies on
// (`len(v.source()) > 0`).
func cloneLinters(src []lint.Linter) []lint.Linter {
	if len(src) == 0 {
		return nil
	}
	dst := make([]lint.Linter, len(src))
	copy(dst, src)
	return dst
}

// StyleResult holds the resolved style configuration and the effective
// linters to run during post-task review.
type StyleResult struct {
	// Config is the merged style configuration (all available styles).
	// Always non-nil — builtins are loaded even when no config files exist.
	Config *styleconfig.Config
	// Resolved is the active style after merge. Nil when no style is active.
	Resolved *styleconfig.Resolved
	// AgentStyle is the prompt-ready data for the agent. Nil when no style is active.
	AgentStyle *prompts.CodingStyleData
	// Linters is the effective set of linters for the active style. It is
	// either the style's configured lint_cmd wrapped as raw adapters, or
	// DefaultLinters when the style has no explicit lint_cmd. Nil when no
	// style is active or no linter is available.
	Linters []lint.Linter
	// DefaultLinters is the auto-detected linter set for the project language.
	// Used as fallback when a style has no explicit lint_cmd (e.g. during
	// runtime style cycling).
	DefaultLinters []lint.Linter
	// Architecture is the read port consumed by the architecture
	// validator at edit time. Always non-nil so callers can register
	// the validator unconditionally; the validator short-circuits via
	// its Applicable check when no caps are configured. Use
	// [StyleResult.SetArchitecture] to update the active caps when the
	// developer cycles styles at runtime — the read port and the write
	// hook share thread-safe state under the hood.
	Architecture architecture.Provider
	// SetArchitecture updates the active architecture caps. Composition
	// roots wire this into runtime style-cycle handlers (e.g. Alt+S in
	// the TUI). Pass an empty styleconfig.Architecture to disable caps.
	SetArchitecture func(styleconfig.Architecture)
	// PerFileLinters is the thread-safe holder for the per-file safe
	// linter subset, used by the pre-approval lint validator stage.
	// Always non-nil so callers can register the validator
	// unconditionally; the holder returns nil when neither the
	// style nor the project has a per-file linter configured. The
	// TUI's CycleStyle closure updates the holder on style switch
	// so a style with a different {file}-bearing lint_cmd takes
	// effect on the next edit without rebuilding the pipeline.
	PerFileLinters *PerFileLinterHolder
	// DefaultPerFileLinters is the auto-detected per-file linter set for
	// the project language. Used as fallback when a style has no
	// {file}-bearing lint_cmd, including during runtime style cycling.
	DefaultPerFileLinters []lint.Linter
}

// NewStyle resolves the coding style configuration and converts it to
// agent-ready prompt data. Returns a zero StyleResult except for the
// Architecture provider, which is always non-nil so validators can be
// registered unconditionally at the composition root.
func NewStyle(projectRoot string) StyleResult {
	cfg, resolved := styleconfig.Resolve(projectRoot)

	// Auto-detect project-appropriate linters. Stored on the result so
	// callers can reuse them during runtime style cycling.
	defaults := lint.Detect(projectRoot)
	perFileDefaults := lint.DetectPerFile(projectRoot)

	archProvider := styleconfig.NewActiveProvider()
	perFileHolder := NewPerFileLinterHolder(nil)

	setArch := func(a styleconfig.Architecture) { archProvider.Set(a) }

	if resolved == nil {
		slog.Debug("wire: no active coding style")
		return StyleResult{
			Config:                cfg,
			DefaultLinters:        defaults,
			DefaultPerFileLinters: perFileDefaults,
			Architecture:          archProvider,
			SetArchitecture:       setArch,
			PerFileLinters:        perFileHolder,
		}
	}

	archProvider.Set(resolved.Architecture)
	perFileHolder.Set(LintersForStylePerFile(resolved.LintCmd, perFileDefaults))

	slog.Info("wire: coding style active", "style", resolved.Name)

	return StyleResult{
		Config:                cfg,
		Resolved:              resolved,
		AgentStyle:            prompts.NewCodingStyleData(resolved.Name, ConvertRules(resolved.Rules)),
		Linters:               LintersForStyle(resolved.LintCmd, defaults),
		DefaultLinters:        defaults,
		PerFileLinters:        perFileHolder,
		DefaultPerFileLinters: perFileDefaults,
		Architecture:          archProvider,
		SetArchitecture:       setArch,
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

// LintersForStylePerFile returns the per-file safe subset of linters for a
// style — used by the pre-approval lint validator stage. If the style has
// lint_cmd, only commands containing the {file} placeholder qualify;
// otherwise the auto-detected per-file defaults (luacheck and similar) are
// returned. Returns nil when neither source produces a per-file linter so
// the validator's Applicable check skips the stage cleanly.
func LintersForStylePerFile(lintCmd []string, perFileDefaults []lint.Linter) []lint.Linter {
	if perFile := lint.PerFileShellCommands(lintCmd); len(perFile) > 0 {
		return perFile
	}
	return perFileDefaults
}

// ConvertRules translates styleconfig rules into prompt-ready StyleRule values.
// Used by NewStyle and by the TUI for runtime style switching.
func ConvertRules(rules []styleconfig.Rule) []prompts.StyleRule {
	out := make([]prompts.StyleRule, len(rules))
	for i, r := range rules {
		out[i] = prompts.StyleRule{Name: r.Name, Instruction: r.Instruction, Enforcement: r.Enforcement}
	}
	return out
}
