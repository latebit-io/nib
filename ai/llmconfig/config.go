// Package llmconfig loads and merges LLM provider configuration from
// global and project-level config files, with environment variable overrides.
package llmconfig

import (
	"slices"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
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
	// PromptCaching enables Anthropic-style cache_control annotations on
	// messages and tools. Reduces repeated input token costs by ~90% for
	// providers that support it (e.g., Anthropic models via OpenRouter).
	// Use a pointer to distinguish "not set" from "explicitly false" during
	// config merging — nil means inherit from the lower-priority layer.
	PromptCaching *bool `json:"prompt_caching,omitempty"`
	// OAuthProvider identifies the OAuth provider for subscription-based auth
	// (e.g., "openai" for ChatGPT, "copilot" for GitHub Copilot).
	// When set, auth uses OAuth tokens instead of the APIKeyEnv static key.
	OAuthProvider string `json:"oauth_provider,omitempty"`
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
	// PromptCaching indicates whether cache_control annotations should be
	// added to LLM requests.
	PromptCaching bool
	// OAuthProvider identifies the OAuth provider when using subscription-based
	// auth (e.g., "openai", "copilot"). Empty means static API key auth.
	OAuthProvider string
	// Auth is an optional pre-configured authenticator. When set, NewProvider
	// uses it instead of creating a StaticKeyAuth from apiKey. This is set
	// by the OAuth wiring layer after a successful login.
	Auth llm.Auth
	// Effort is the reasoning effort providers apply (low|medium|high|xhigh|
	// max). Empty means the provider default. Set on a cloned Resolved to
	// build a provider with a specific effort (e.g. a subagent's override),
	// mirroring how Model is overridden. Honored by the OpenAI-style
	// providers; the Anthropic provider ignores it until extended-thinking
	// support lands.
	Effort string
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
// an LLM provider. API key takes priority; OAuth is the fallback.
func (r *Resolved) HasProvider() bool {
	if r.apiKey != "" {
		return true
	}
	return r.OAuthProvider != "" && r.Auth != nil
}

// SetAPIKey sets the API key on the resolved configuration.
// Used by the key store to inject TUI-entered keys. No-op for OAuth profiles.
func (r *Resolved) SetAPIKey(key string) {
	if r.OAuthProvider == "" {
		r.apiKey = key
	}
}

// NewProvider creates an LLM provider from the resolved configuration.
// Profiles with both OAuthProvider and APIKeyEnv prefer the API key when
// available, falling back to OAuth. OAuth-only profiles require Auth.
// Returns nil if insufficient configuration exists.
func (r *Resolved) NewProvider() llm.Provider {
	// Try API key first — works for both pure API-key profiles and
	// dual-mode profiles (like anthropic) where the key takes priority.
	effort := llm.Effort(r.Effort)
	if r.apiKey != "" {
		if r.isAnthropicEndpoint() {
			return llm.NewAnthropicAPI(r.BaseURL, r.Model, llm.AnthropicKeyAuth(r.apiKey), r.PromptCaching)
		}
		return llm.NewAgentAPI(r.BaseURL, r.Model, llm.StaticKeyAuth(r.apiKey), r.PromptCaching, effort)
	}

	// Fall back to OAuth if configured and authenticated.
	if r.OAuthProvider != "" && r.Auth != nil {
		if r.OAuthProvider == "openai" {
			return llm.NewCodexAPI(r.Model, r.Auth, effort)
		}
		if r.OAuthProvider == "anthropic" {
			return llm.NewAnthropicAPI(r.BaseURL, r.Model, r.Auth, r.PromptCaching)
		}
		return llm.NewAgentAPI(r.BaseURL, r.Model, r.Auth, r.PromptCaching, effort)
	}

	return nil
}

// isAnthropicEndpoint reports whether the resolved base URL points to
// the Anthropic API, which requires the Messages API wire format.
func (r *Resolved) isAnthropicEndpoint() bool {
	return strings.Contains(r.BaseURL, "anthropic.com")
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
