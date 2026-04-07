// Package llmconfig loads and merges LLM provider configuration from
// global and project-level config files, with environment variable overrides.
package llmconfig

import (
	"slices"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
)

// Profile holds the configuration for a single LLM provider endpoint.
type Profile struct {
	BaseURL   string `json:"base_url,omitempty"`
	Model     string `json:"model,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"` // env var name, never the key itself
}

// Config represents the on-disk shape of an LLM configuration file.
// Both global and project files share this format. Fields left unset
// are inherited from lower-priority sources during merge.
type Config struct {
	Profiles map[string]Profile `json:"profiles,omitempty"`
	Active   string             `json:"active,omitempty"`
}

// Resolved holds the final merged configuration with all values populated.
// Produced by [Resolve]; consumed by wire and session.
// The API key is kept private — use [Resolved.NewProvider] to create a
// provider, or [Resolved.HasProvider] to check availability.
type Resolved struct {
	BaseURL   string // full endpoint URL
	Model     string // model identifier sent to the provider
	apiKey    string // actual secret resolved from the environment (private)
	APIKeyEnv string // which env var supplied the key (for diagnostics)
	Profile   string // active profile name ("env" if env-var-only)
}

// DisplayModel returns a short label suitable for the status bar.
// Strips any provider prefix: "google/gemini-2.5-flash" → "gemini-2.5-flash".
func (r *Resolved) DisplayModel() string {
	if i := strings.LastIndex(r.Model, "/"); i >= 0 {
		return r.Model[i+1:]
	}
	return r.Model
}

// HasProvider reports whether enough configuration exists to create
// an LLM provider (at minimum, a non-empty API key).
func (r *Resolved) HasProvider() bool { return r.apiKey != "" }

// NewProvider creates an LLM provider from the resolved configuration.
// Returns nil if no API key is available.
func (r *Resolved) NewProvider() llm.Provider {
	if r.apiKey == "" {
		return nil
	}
	return llm.NewAgentAPI(r.BaseURL, r.Model, r.apiKey)
}

// ProfileNames returns the sorted list of profile names in the config.
// Returns nil if no profiles are defined.
func (c *Config) ProfileNames() []string {
	if len(c.Profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
