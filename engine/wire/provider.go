// Package wire provides shared composition logic used by both the TUI
// and headless agent binaries. It wires together LLM providers, memory
// servers, LSP, and MCP tools from environment variables and project config.
package wire

import (
	"log/slog"

	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/llmconfig"
	"github.com/latebit-io/junto/engine/oauth"
)

// ProviderResult holds the provider, config, and OAuth store from NewProvider.
type ProviderResult struct {
	// Provider is the LLM provider (nil if no auth available).
	Provider llm.Provider
	// Config is the merged LLM configuration with all profiles.
	Config *llmconfig.Config
	// Resolved is the active profile with all values populated.
	Resolved *llmconfig.Resolved
	// OAuthStore is the token store for OAuth-based providers.
	OAuthStore *oauth.Store
}

// NewProvider creates an LLM provider from the merged configuration.
// Returns (nil, config, resolved) if no API key is available — the caller
// checks the provider for nil to decide whether to enable agent features.
// The Config and Resolved are always returned so callers can display model
// info and support runtime profile switching even when the provider is nil.
func NewProvider(projectRoot string) *ProviderResult {
	cfg, resolved := llmconfig.Resolve(projectRoot)

	// Create OAuth store for subscription-based providers.
	store, err := oauth.NewStore(oauth.DefaultStorePath())
	if err != nil {
		slog.Warn("wire: failed to create OAuth store", "err", err)
		// Continue without OAuth — API key profiles still work.
		return &ProviderResult{
			Provider: resolved.NewProvider(),
			Config:   cfg,
			Resolved: resolved,
		}
	}

	// If the resolved profile uses OAuth, wire up the authenticator.
	wireOAuth(resolved, store)

	return &ProviderResult{
		Provider:   resolved.NewProvider(),
		Config:     cfg,
		Resolved:   resolved,
		OAuthStore: store,
	}
}

// WireOAuthProfile sets up OAuth authentication on a resolved profile
// if it has stored tokens. Returns true if auth was wired.
func WireOAuthProfile(resolved *llmconfig.Resolved, store *oauth.Store) bool {
	return wireOAuth(resolved, store)
}

// wireOAuth checks if a resolved profile needs OAuth and has stored tokens.
// If so, it sets the Auth field. Returns true if auth was wired.
func wireOAuth(resolved *llmconfig.Resolved, store *oauth.Store) bool {
	if resolved == nil || store == nil || resolved.OAuthProvider == "" {
		return false
	}

	providerID := oauth.ProviderID(resolved.OAuthProvider)
	if !store.HasToken(providerID) {
		return false
	}

	switch providerID {
	case oauth.ProviderOpenAI:
		ts := oauth.NewOpenAITokenSource(store)
		resolved.Auth = oauth.NewAuthenticator(ts)
		return true
	case oauth.ProviderCopilot:
		ts := oauth.NewCopilotTokenSource(store)
		resolved.Auth = oauth.NewAuthenticator(ts)
		return true
	default:
		slog.Warn("wire: unknown OAuth provider", "provider", resolved.OAuthProvider)
		return false
	}
}
