package styleconfig

import (
	"strings"
	"sync"
)

// Architecture defines deterministic structural caps that complement the
// LLM-evaluated [Rule] set. Caps are language-agnostic but only apply to
// files the project's grammar registry understands — configuration,
// documentation, and unsupported source languages are exempt.
//
// Architecture rules are checked by a pre-approval validator
// (engine/validate/architecture). They run on every proposed edit so a
// drifting file size or function count is caught before the proposal
// reaches the developer rather than as a post-hoc cleanup pass.
type Architecture struct {
	// MaxFileLines is the upper bound on total lines in a single source
	// file. Zero disables the check.
	MaxFileLines int `json:"max_file_lines,omitempty"`

	// MaxFunctionLines is the upper bound on lines inside one function or
	// method. Requires a tree-sitter grammar to detect function bounds —
	// the check is silently skipped for unsupported languages. Zero
	// disables.
	MaxFunctionLines int `json:"max_function_lines,omitempty"`

	// MaxFunctionsPerFile caps the number of functions or methods declared
	// in a single file — a proxy for "this file has too many
	// responsibilities." Requires a tree-sitter grammar; silently skipped
	// otherwise. Zero disables.
	MaxFunctionsPerFile int `json:"max_functions_per_file,omitempty"`

	// Action selects how cap violations are surfaced. One of:
	//
	//   warn  — Verdict=Retry; the LLM gets one chance per edit to fix.
	//   block — Verdict=Block; the proposal surfaces to the developer
	//           without silent retry.
	//   off   — the architecture validator is disabled regardless of cap
	//           fields.
	//
	// Empty defaults to "warn". Comparisons are case-insensitive.
	Action string `json:"action,omitempty"`
}

// Enabled reports whether any architectural cap is active. Returns false
// when Action is "off" or every cap field is zero — the validator's
// Applicable check uses this to skip the stage entirely.
func (a Architecture) Enabled() bool {
	if a.normalisedAction() == "off" {
		return false
	}
	return a.MaxFileLines > 0 || a.MaxFunctionLines > 0 || a.MaxFunctionsPerFile > 0
}

// IsBlock reports whether cap violations should produce Verdict=Block
// rather than Verdict=Retry. Defaults to false ("warn").
func (a Architecture) IsBlock() bool {
	return a.normalisedAction() == "block"
}

func (a Architecture) normalisedAction() string {
	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "block":
		return "block"
	case "off":
		return "off"
	default:
		return "warn"
	}
}

// ActiveProvider is a thread-safe holder for the currently active
// architecture configuration. The validator, the agent, and the TUI's
// style-cycle path share one provider so a runtime style switch is
// reflected in the next validation without rebuilding the pipeline.
//
// The TUI updates via [ActiveProvider.Set] on the Bubble Tea goroutine;
// validators read via [ActiveProvider.Architecture] from the agent
// goroutine. The internal lock keeps both in sync without leaking
// concurrency primitives into callers.
type ActiveProvider struct {
	mu   sync.RWMutex
	arch Architecture
}

// NewActiveProvider returns a provider holding the zero-value
// Architecture (Enabled returns false). Composition roots populate it
// from the resolved style at startup and on every style cycle.
func NewActiveProvider() *ActiveProvider {
	return &ActiveProvider{}
}

// Architecture returns a snapshot of the active configuration. The
// returned value is a copy — callers may not mutate the provider through
// it.
func (p *ActiveProvider) Architecture() Architecture {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.arch
}

// Set replaces the active configuration. Pass the zero value to disable
// architecture validation, for example when the developer cycles past
// the last style.
func (p *ActiveProvider) Set(a Architecture) {
	p.mu.Lock()
	p.arch = a
	p.mu.Unlock()
}
