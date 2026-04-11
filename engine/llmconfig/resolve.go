package llmconfig

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	// DefaultBaseURL is the LLM provider base URL when nothing else is configured.
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	// DefaultModel is the LLM model when nothing else is configured.
	DefaultModel = "google/gemini-2.5-flash"
	// DefaultKeyEnv is the environment variable checked for the API key
	// when no api_key_env is set in any config file.
	DefaultKeyEnv = "LLM_API_KEY"
)

// builtinProfiles are always available — user config files can override them.
var builtinProfiles = map[string]Profile{
	"openrouter": {
		BaseURL:       "https://openrouter.ai/api/v1",
		Model:         "google/gemini-2.5-flash",
		APIKeyEnv:     "OPENROUTER_API_KEY",
		PromptCaching: ptrBool(true),
	},
	"gemini": {
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
		Model:     "gemini-2.5-flash",
		APIKeyEnv: "GEMINI_API_KEY",
	},
	"minimax": {
		BaseURL:   "https://api.minimax.io/v1",
		Model:     "MiniMax-M2.7",
		APIKeyEnv: "MINIMAX_API_KEY",
	},
}

// builtinFallbackOrder is the priority when auto-selecting a built-in profile
// because the active profile has no API key. First match wins.
var builtinFallbackOrder = []string{"gemini", "minimax", "openrouter"}

// ptrBool returns a pointer to a bool value.
func ptrBool(b bool) *bool { return &b }

// Resolve loads and merges LLM configuration from all sources.
// The merge order (each layer overrides the previous):
//
//  1. Hardcoded defaults
//  2. Global config file (<UserConfigDir>/junto/llm.json)
//  3. Project config file (<projectRoot>/.project/llm.json)
//  4. Environment variables (LLM_BASE_URL, LLM_MODEL, LLM_API_KEY)
//
// Missing files are silently skipped. Parse errors are logged and skipped.
// The projectRoot parameter may be empty to skip project-level config.
func Resolve(projectRoot string) (*Config, *Resolved) {
	return resolveWithPaths(GlobalConfigPath(), projectRoot)
}

// resolveWithPaths is the internal implementation that accepts an explicit
// global config path, allowing tests to inject a temp directory.
func resolveWithPaths(globalPath, projectRoot string) (*Config, *Resolved) {
	cfg := &Config{Profiles: make(map[string]Profile)}

	// Seed built-in profiles first — file configs merge on top.
	for name, p := range builtinProfiles {
		cfg.Profiles[name] = p
	}

	if g := loadFile(globalPath); g != nil {
		mergeConfigs(cfg, g)
	}
	if projectRoot != "" {
		if p := loadFile(projectConfigPath(projectRoot)); p != nil {
			mergeConfigs(cfg, p)
		}
	}

	return cfg, resolve(cfg)
}

// ResolveProfile resolves a specific named profile from a merged Config.
// Returns nil if the profile doesn't exist. Use [Resolved.HasProvider] to
// check whether the returned config has an API key.
func ResolveProfile(cfg *Config, name string) *Resolved {
	p, ok := cfg.Profiles[name]
	if !ok {
		return nil
	}
	r := &Resolved{
		BaseURL:   p.BaseURL,
		Model:     p.Model,
		APIKeyEnv: p.APIKeyEnv,
		Profile:   name,
	}
	if p.PromptCaching != nil {
		r.PromptCaching = *p.PromptCaching
	}
	if r.BaseURL == "" {
		r.BaseURL = DefaultBaseURL
	}
	if r.Model == "" {
		r.Model = DefaultModel
	}
	if r.APIKeyEnv == "" {
		r.APIKeyEnv = DefaultKeyEnv
	}
	r.apiKey = os.Getenv(r.APIKeyEnv)

	// Environment variable overrides — same precedence as Resolve().
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		r.BaseURL = v
	}
	if v := os.Getenv("LLM_MODEL"); v != "" {
		r.Model = v
	}
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		r.APIKeyEnv = DefaultKeyEnv
		r.apiKey = v
	}
	return r
}

// applyProfile copies non-zero fields from a Profile into a Resolved.
func applyProfile(r *Resolved, p Profile, name string) {
	r.Profile = name
	if p.BaseURL != "" {
		r.BaseURL = p.BaseURL
	}
	if p.Model != "" {
		r.Model = p.Model
	}
	if p.APIKeyEnv != "" {
		r.APIKeyEnv = p.APIKeyEnv
	}
	if p.PromptCaching != nil {
		r.PromptCaching = *p.PromptCaching
	}
}

// resolve converts a merged Config into a Resolved by looking up the active
// profile and applying environment variable overrides.
func resolve(cfg *Config) *Resolved {
	r := &Resolved{
		BaseURL:   DefaultBaseURL,
		Model:     DefaultModel,
		APIKeyEnv: DefaultKeyEnv,
		Profile:   "env",
	}

	// Look up active profile.
	if cfg.Active != "" {
		if p, ok := cfg.Profiles[cfg.Active]; ok {
			applyProfile(r, p, cfg.Active)
		} else {
			slog.Warn("llmconfig: active profile not found", "profile", cfg.Active)
		}
	}

	// Resolve API key from the named env var.
	r.apiKey = os.Getenv(r.APIKeyEnv)

	// Resolve API key from LLM_API_KEY override.
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		r.APIKeyEnv = DefaultKeyEnv
		r.apiKey = v
	}

	// Auto-fallback: if no API key yet, try built-in profiles in priority order.
	// Uses cfg.Profiles (not builtinProfiles) so file overrides are respected.
	if r.apiKey == "" {
		for _, name := range builtinFallbackOrder {
			p, ok := cfg.Profiles[name]
			if !ok {
				continue
			}
			if key := os.Getenv(p.APIKeyEnv); key != "" {
				applyProfile(r, p, name)
				r.apiKey = key
				break
			}
		}
	}

	// Environment variable overrides (highest priority) — applied after
	// fallback so they can't be clobbered by it.
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		r.BaseURL = v
	}
	if v := os.Getenv("LLM_MODEL"); v != "" {
		r.Model = v
	}

	return r
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
			slog.Warn("llmconfig: cannot read config", "path", path, "err", err)
		}
		return nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("llmconfig: invalid config JSON", "path", path, "err", err)
		return nil
	}
	return &cfg
}

// mergeConfigs overlays src onto dst. Profiles are merged by name:
// existing profiles get non-empty fields overwritten, new profiles are added.
// Active is overridden if non-empty in src.
func mergeConfigs(dst, src *Config) {
	if src.Active != "" {
		dst.Active = src.Active
	}
	if dst.Profiles == nil {
		dst.Profiles = make(map[string]Profile)
	}
	for name, sp := range src.Profiles {
		dp := dst.Profiles[name]
		if sp.BaseURL != "" {
			dp.BaseURL = sp.BaseURL
		}
		if sp.Model != "" {
			dp.Model = sp.Model
		}
		if sp.APIKeyEnv != "" {
			dp.APIKeyEnv = sp.APIKeyEnv
		}
		if sp.PromptCaching != nil {
			dp.PromptCaching = sp.PromptCaching
		}
		dst.Profiles[name] = dp
	}
}

// GlobalConfigPath returns <UserConfigDir>/junto/llm.json.
// Returns empty string if the user config directory cannot be resolved.
func GlobalConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		slog.Warn("llmconfig: cannot resolve user config dir", "err", err)
		return ""
	}
	return filepath.Join(dir, "junto", "llm.json")
}

// projectConfigPath returns <projectRoot>/.project/llm.json.
func projectConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".project", "llm.json")
}
