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
	return resolveWithPaths(globalConfigPath(), projectRoot)
}

// resolveWithPaths is the internal implementation that accepts an explicit
// global config path, allowing tests to inject a temp directory.
func resolveWithPaths(globalPath, projectRoot string) (*Config, *Resolved) {
	cfg := &Config{Profiles: make(map[string]Profile)}

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
// Returns nil if the profile doesn't exist or has no API key.
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
		r.apiKey = v
	}
	return r
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
			r.Profile = cfg.Active
			if p.BaseURL != "" {
				r.BaseURL = p.BaseURL
			}
			if p.Model != "" {
				r.Model = p.Model
			}
			if p.APIKeyEnv != "" {
				r.APIKeyEnv = p.APIKeyEnv
			}
		} else {
			slog.Warn("llmconfig: active profile not found", "profile", cfg.Active)
		}
	}

	// Resolve API key from the named env var.
	r.apiKey = os.Getenv(r.APIKeyEnv)

	// Environment variable overrides (highest priority).
	if v := os.Getenv("LLM_BASE_URL"); v != "" {
		r.BaseURL = v
	}
	if v := os.Getenv("LLM_MODEL"); v != "" {
		r.Model = v
	}
	if v := os.Getenv("LLM_API_KEY"); v != "" {
		r.apiKey = v
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
		dst.Profiles[name] = dp
	}
}

// globalConfigPath returns <UserConfigDir>/junto/llm.json.
func globalConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "junto", "llm.json")
}

// projectConfigPath returns <projectRoot>/.project/llm.json.
func projectConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".project", "llm.json")
}
