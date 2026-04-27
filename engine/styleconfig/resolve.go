package styleconfig

import (
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

//go:embed styles/*.json
var builtinStyles embed.FS

// Resolve loads and merges style configuration from all sources.
// The merge order (each layer overrides the previous):
//
//  1. Embedded built-in styles
//  2. Global config file (<UserConfigDir>/junto/style.json)
//  3. Project config file (<projectRoot>/.project/style.json)
//  4. JUNTO_STYLE environment variable (overrides active style name)
//
// Missing files are silently skipped. Parse errors are logged and skipped.
// Config is always non-nil (builtins are loaded even without config files).
// Resolved is nil when no style is active.
func Resolve(projectRoot string) (*Config, *Resolved) {
	return resolveWithPaths(globalConfigPath(), projectRoot)
}

// resolveWithPaths is the internal implementation of [Resolve] that accepts
// an explicit global config path, allowing tests to inject a temp directory
// instead of the real user config directory.
func resolveWithPaths(globalPath, projectRoot string) (*Config, *Resolved) {
	cfg := loadBuiltins()

	if g := loadFile(globalPath); g != nil {
		mergeConfigs(cfg, g)
	}
	if projectRoot != "" {
		if p := loadFile(projectConfigPath(projectRoot)); p != nil {
			mergeConfigs(cfg, p)
		}
	}

	// Environment variable override for active style.
	if v := os.Getenv("JUNTO_STYLE"); v != "" {
		cfg.Active = v
	}

	return cfg, resolve(cfg)
}

// resolve converts a merged Config into a Resolved by looking up the active
// style. Returns nil when no style is active or the named style is not found.
func resolve(cfg *Config) *Resolved {
	if cfg.Active == "" {
		return nil
	}

	s, ok := cfg.Styles[cfg.Active]
	if !ok {
		slog.Warn("styleconfig: active style not found", "style", cfg.Active,
			"available", cfg.StyleNames())
		return nil
	}

	return &Resolved{
		Name:           s.Name,
		Rules:          s.Rules,
		LintCmd:        s.LintCmd,
		Evaluator:      s.Evaluator,
		EvaluatorModel: s.EvaluatorModel,
		Architecture:   s.Architecture,
	}
}

// defaultActiveStyle is the built-in style activated when no config file or
// environment variable selects a different one. File configs and JUNTO_STYLE
// override this.
//
// Empty means "no active style" — the prompt skips the style section, the
// architecture validator and style evaluator are disabled, and per-edit
// linting depends only on auto-detected per-file linters. This is the
// production default since 2026-04-26: governance is opt-in. Set
// JUNTO_STYLE=clean-code (or similar) or write `.project/style.json` with
// an `active` field to opt into a style.
const defaultActiveStyle = ""

// loadBuiltins reads all embedded style JSON files into a Config.
func loadBuiltins() *Config {
	cfg := &Config{Styles: make(map[string]Style)}

	entries, err := builtinStyles.ReadDir("styles")
	if err != nil {
		// Embedded files are compiled in — this should never happen.
		slog.Error("styleconfig: cannot read embedded styles", "err", err)
		return cfg
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := builtinStyles.ReadFile("styles/" + entry.Name())
		if err != nil {
			slog.Error("styleconfig: cannot read embedded style", "file", entry.Name(), "err", err)
			continue
		}
		var s Style
		if err := json.Unmarshal(data, &s); err != nil {
			slog.Error("styleconfig: invalid embedded style JSON", "file", entry.Name(), "err", err)
			continue
		}

		warnInvalidEnforcement(entry.Name(), s.Rules)
		warnInvalidArchitectureAction(entry.Name(), s.Name, s.Architecture.Action)

		// Key is the filename without extension: "solid-hexagonal.json" → "solid-hexagonal".
		key := strings.TrimSuffix(entry.Name(), ".json")
		cfg.Styles[key] = s
	}

	// Activate the default style if one is configured AND it was loaded
	// successfully. When defaultActiveStyle is empty (the production default
	// since 2026-04-26), Active stays "" — meaning no style is active and
	// the architecture validator, style evaluator, and prompt style section
	// are all disabled. The developer opts in via JUNTO_STYLE or
	// .project/style.json.
	//
	// retained for future reactivation if defaultActiveStyle policy
	// changes — the body is dormant today (the constant is "") but kept
	// so flipping the constant back to a style name (e.g. "clean-code")
	// is a single-line change. Removing the block would require
	// re-deriving and re-testing the activation+fallback semantics.
	if defaultActiveStyle != "" {
		if _, ok := cfg.Styles[defaultActiveStyle]; ok {
			cfg.Active = defaultActiveStyle
		} else if names := cfg.StyleNames(); len(names) > 0 {
			cfg.Active = names[0]
			slog.Warn("styleconfig: default active style missing; falling back",
				"default", defaultActiveStyle, "fallback", cfg.Active)
		}
	}

	return cfg
}

// warnInvalidEnforcement logs a warning for any rule whose Enforcement value
// is not "hard" or "soft". Invalid values are not blocked — they are passed
// through to the prompt — but the developer should know about the misconfiguration.
func warnInvalidEnforcement(source string, rules []Rule) {
	for i, r := range rules {
		if r.Enforcement != "hard" && r.Enforcement != "soft" {
			slog.Warn("styleconfig: invalid enforcement value",
				"source", source, "rule", r.Name, "index", i,
				"enforcement", r.Enforcement, "expected", "hard|soft")
		}
	}
}

// isValidArchitectureAction reports whether action is one of the
// known values, applying the same normalisation
// (lowercase + trim) the consumer's [Architecture.normalisedAction]
// uses. Without this, a project config with "Action": "BLOCK" would
// be rejected by the merge path and silently fall back to whichever
// value the builtin shipped — even though the architecture struct's
// own contract says comparisons are case-insensitive. Empty is
// treated as valid because empty defaults to "warn" downstream.
func isValidArchitectureAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "warn", "block", "off":
		return true
	}
	return false
}

// canonicalArchitectureAction returns the lowercase trimmed form of
// action so merged configs store a single canonical representation.
// Pass-through "" for unset values; callers must check before using
// the result as a non-empty signal.
func canonicalArchitectureAction(action string) string {
	return strings.ToLower(strings.TrimSpace(action))
}

// warnInvalidArchitectureAction logs a warning when source declares an
// architecture.action that is not one of the known values. The merge
// path additionally REFUSES to overwrite a valid builtin with an
// invalid value (see [mergeConfigs]) — a typo in a project config
// must not silently demote `block` to the default `warn`, which
// would change UX behaviour (silent retry vs surface to developer)
// rather than just prompt wording.
func warnInvalidArchitectureAction(source, styleName, action string) {
	if isValidArchitectureAction(action) {
		return
	}
	slog.Warn("styleconfig: invalid architecture action",
		"source", source, "style", styleName,
		"action", action, "expected", "warn|block|off")
}

// loadFile reads and parses a single config file.
// Returns nil on missing file or parse error.
func loadFile(path string) *Config {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("styleconfig: cannot read config", "path", path, "err", err)
		}
		return nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("styleconfig: invalid config JSON", "path", path, "err", err)
		return nil
	}
	for name, s := range cfg.Styles {
		warnInvalidEnforcement(path+":"+name, s.Rules)
		warnInvalidArchitectureAction(path+":"+name, name, s.Architecture.Action)
	}
	return &cfg
}

// mergeConfigs overlays src onto dst. Styles are merged by name:
// existing styles get non-empty fields overwritten, new styles are added.
// Active is overridden if non-empty in src.
func mergeConfigs(dst, src *Config) {
	if src.Active != "" {
		dst.Active = src.Active
	}
	if dst.Styles == nil {
		dst.Styles = make(map[string]Style)
	}
	for name, ss := range src.Styles {
		ds := dst.Styles[name]
		if ss.Name != "" {
			ds.Name = ss.Name
		}
		if ss.Description != "" {
			ds.Description = ss.Description
		}
		if len(ss.Rules) > 0 {
			ds.Rules = ss.Rules
		}
		if len(ss.LintCmd) > 0 {
			ds.LintCmd = ss.LintCmd
		}
		if ss.Evaluator {
			ds.Evaluator = true
		}
		if ss.EvaluatorModel != "" {
			ds.EvaluatorModel = ss.EvaluatorModel
		}
		// Architecture merges field-by-field so a project file can tighten
		// one cap without forcing the developer to repeat the others.
		if ss.Architecture.MaxFileLines > 0 {
			ds.Architecture.MaxFileLines = ss.Architecture.MaxFileLines
		}
		if ss.Architecture.MaxFunctionLines > 0 {
			ds.Architecture.MaxFunctionLines = ss.Architecture.MaxFunctionLines
		}
		if ss.Architecture.MaxFunctionsPerFile > 0 {
			ds.Architecture.MaxFunctionsPerFile = ss.Architecture.MaxFunctionsPerFile
		}
		// Action overwrites only with a known value, normalised to
		// lowercase canonical form. An invalid action (typo in
		// project/global config) would otherwise silently replace a
		// builtin "block" with garbage, which normalisedAction maps
		// to the default "warn" — silently demoting Block→Retry is
		// a UX regression we refuse to accept. The load path
		// already emitted a warn-level log at file load via
		// warnInvalidArchitectureAction; this branch is the safety
		// net. Canonicalising at write time means downstream
		// consumers can compare strings directly without re-running
		// normalisedAction in every read site.
		//
		// Canonicalise FIRST, then gate. A whitespace-only string
		// passes isValidArchitectureAction (which trims internally
		// and accepts "") but canonicalises to "" — the raw-string
		// check missed it and we'd silently clear an inherited
		// policy.
		if action := canonicalArchitectureAction(ss.Architecture.Action); action != "" && isValidArchitectureAction(action) {
			ds.Architecture.Action = action
		}
		dst.Styles[name] = ds
	}
}

// globalConfigPath returns <UserConfigDir>/junto/style.json.
// Returns empty string if the user config directory cannot be resolved.
func globalConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		slog.Warn("styleconfig: cannot resolve user config dir", "err", err)
		return ""
	}
	return filepath.Join(dir, "junto", "style.json")
}

// projectConfigPath returns <projectRoot>/.project/style.json.
func projectConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".project", "style.json")
}
