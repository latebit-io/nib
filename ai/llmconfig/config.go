// Package llmconfig loads and merges LLM provider configuration from
// global and project-level config files, with environment variable overrides.
package llmconfig

import (
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/oauth"
)

// Profile holds the configuration for a single LLM provider endpoint.
type Profile struct {
	// BaseURL is the provider's API base URL (e.g., "https://api.minimax.io/v1").
	BaseURL string `json:"base_url,omitempty"`
	// Model is the model identifier (e.g., "gemini-3.8-flash").
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
	// API is the wire format the endpoint speaks: [APIChat], [APIMessages]
	// or [APIResponses]. Empty picks one from the base URL and sign-in.
	API string `json:"api,omitempty"`
}

// Wire formats a [Profile] can name in API.
const (
	// APIChat is OpenAI-compatible chat completions (/chat/completions).
	APIChat = "chat"
	// APIMessages is the Anthropic Messages API (/v1/messages).
	APIMessages = "messages"
	// APIResponses is Open Responses (/responses), the Responses API format.
	APIResponses = "responses"
)

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
	// API is the profile's wire format; empty means picked by heuristic.
	API string
	// Effort is the reasoning effort providers apply (low|medium|high|xhigh|
	// max). Empty means the provider default. Set on a cloned Resolved to
	// build a provider with a specific effort (e.g. a subagent's override),
	// mirroring how Model is overridden. OpenAI-style providers map it to
	// reasoning_effort; the Anthropic provider maps it to an extended-
	// thinking budget.
	Effort string
}

// DisplayModel returns a short display name for the model.
// Strips any provider prefix: "google/gemini-3.8-flash" → "gemini-3.8-flash".
func (r *Resolved) DisplayModel() string {
	if i := strings.LastIndex(r.Model, "/"); i >= 0 {
		return r.Model[i+1:]
	}
	return r.Model
}

// HasProvider reports whether enough configuration exists to create
// an LLM provider: credentials (an API key, else OAuth) and a
// configuration [Resolved.Validate] accepts.
func (r *Resolved) HasProvider() bool {
	if r.apiKey == "" && (r.OAuthProvider == "" || r.Auth == nil) {
		return false
	}
	return r.Validate() == nil
}

// Validate reports what keeps the profile from building a provider other
// than missing credentials: an api nib does not know, a ChatGPT sign-in
// pointed away from its backend, or credentials sent over plain http.
func (r *Resolved) Validate() error {
	if _, ok := r.wireAPI(); !ok {
		return fmt.Errorf("profile %s: unknown api %q (use %s, %s or %s)", r.Profile, r.API, APIChat, APIMessages, APIResponses)
	}
	if r.chatGPT() {
		// Its token is for the Codex backend alone, whatever base_url says.
		if r.API != "" && r.API != APIResponses {
			return fmt.Errorf("profile %s: a ChatGPT sign-in works only with the Codex backend, not api %q", r.Profile, r.API)
		}
		return nil
	}
	if r.apiKey != "" || r.OAuthProvider != "" {
		if err := plainHTTP(r.BaseURL); err != nil {
			return fmt.Errorf("profile %s: %w", r.Profile, err)
		}
	}
	return nil
}

// chatGPT reports whether the profile signs in with ChatGPT rather than
// a key: its credentials then speak to the Codex backend only.
func (r *Resolved) chatGPT() bool {
	return r.apiKey == "" && r.OAuthProvider == string(oauth.ProviderOpenAI)
}

// plainHTTP refuses an http base URL on another machine, where a key
// would cross the network in the clear. A server on this machine, a
// private address or a bare host name on the local network is allowed.
func plainHTTP(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" {
		return nil // https, or a URL the request itself will reject
	}
	host := u.Hostname()
	refused := fmt.Errorf("refusing to send credentials over plain http to %s; use https", host)
	// An address first: an IPv6 one has no dot but may be anywhere.
	// netip takes a zone (fe80::1%eth0), which net.ParseIP refuses.
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.IsLoopback() || ip.IsPrivate() {
			return nil
		}
		return refused
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return nil
	}
	return refused
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
	if !effort.IsValid() {
		// A typo would otherwise silently degrade to the provider default
		// inside the adapter; log it so the misconfiguration is visible.
		slog.Warn("llmconfig: unknown effort, using provider default", "effort", r.Effort)
		effort = ""
	}
	if err := r.Validate(); err != nil {
		slog.Warn("llmconfig: no provider built", "err", err)
		return nil
	}
	api, _ := r.wireAPI() // Validate accepted it
	var auth llm.Auth
	switch {
	case r.apiKey != "" && api == APIMessages:
		auth = llm.AnthropicKeyAuth(r.apiKey)
	case r.apiKey != "":
		auth = llm.StaticKeyAuth(r.apiKey)
	case r.Auth == nil:
		return nil
	case r.chatGPT():
		return llm.NewCodexAPI(r.Model, r.Auth, effort)
	case r.OAuthProvider != "":
		auth = r.Auth
	default:
		return nil
	}
	switch api {
	case APIMessages:
		return llm.NewAnthropicAPI(r.BaseURL, r.Model, auth, r.PromptCaching, effort)
	case APIResponses:
		return llm.NewResponsesAPI(r.BaseURL, r.Model, auth, effort)
	}
	return llm.NewAgentAPI(r.BaseURL, r.Model, auth, r.PromptCaching, effort)
}

// wireAPI returns the wire format to speak: the profile's, or one picked
// from the base URL. ok is false for a name nib does not know.
func (r *Resolved) wireAPI() (api string, ok bool) {
	switch r.API {
	case APIChat, APIMessages, APIResponses:
		return r.API, true
	case "":
		if r.isAnthropicEndpoint() {
			return APIMessages, true
		}
		return APIChat, true
	}
	return "", false
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
	return slices.Sorted(maps.Keys(c.Profiles))
}
