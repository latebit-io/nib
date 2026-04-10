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
	// BaseURL is the provider's API base URL (e.g., "https://api.minimax.io/v1").
	BaseURL string `json:"base_url,omitempty"`
	// Model is the model identifier (e.g., "gemini-2.5-flash").
	Model string `json:"model,omitempty"`
	// APIKeyEnv is the environment variable name holding the API key.
	// Never stores the key itself — only the variable name.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Config represents the on-disk shape of an LLM configuration file.
// Both global and project files share this format. Fields left unset
// are inherited from lower-priority sources during merge.
type Config struct {
	// Profiles maps profile names to their endpoint configurations.
	Profiles map[string]Profile `json:"profiles,omitempty"`
	// Active is the name of the currently selected profile.
	Active string `json:"active,omitempty"`
}

// Resolved holds the final merged configuration with all values populated.
// Produced by [Resolve]; consumed by wire and session.
// The API key is kept private — use [Resolved.NewProvider] to create a
// provider, or [Resolved.HasProvider] to check availability.
type Resolved struct {
	// BaseURL is the full provider endpoint URL.
	BaseURL string
	// Model is the model identifier sent to the provider.
	Model string
	// apiKey is the actual secret resolved from the environment (private).
	apiKey string
	// APIKeyEnv is the environment variable name that supplied the key.
	APIKeyEnv string
	// Profile is the active profile name ("env" if env-var-only).
	Profile string
}

// DisplayModel returns a short display name for the model.
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
