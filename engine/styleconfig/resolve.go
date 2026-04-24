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
const defaultActiveStyle = "clean-code"

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

		// Key is the filename without extension: "solid-hexagonal.json" → "solid-hexagonal".
		key := strings.TrimSuffix(entry.Name(), ".json")
		cfg.Styles[key] = s
	}

	// Activate the default style if it was loaded successfully.
	// Fall back to the first available style (sorted) if the default is missing.
	if _, ok := cfg.Styles[defaultActiveStyle]; ok {
		cfg.Active = defaultActiveStyle
	} else if names := cfg.StyleNames(); len(names) > 0 {
		cfg.Active = names[0]
		slog.Warn("styleconfig: default active style missing; falling back",
			"default", defaultActiveStyle, "fallback", cfg.Active)
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
		if ss.Architecture.Action != "" {
			ds.Architecture.Action = ss.Architecture.Action
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
