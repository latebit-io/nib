// Package wire provides shared composition logic used by both the TUI
// and headless agent binaries. It wires together LLM providers, memory
// servers, LSP, and MCP tools from environment variables and project config.
package wire

import (
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/ai/oauth"
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
	// KeyStore holds API keys entered via the TUI.
	KeyStore *oauth.KeyStore
}

// NewProvider creates an LLM provider from the merged configuration.
// Returns (nil, config, resolved) if no API key is available — the caller
// checks the provider for nil to decide whether to enable agent features.
// The Config and Resolved are always returned so callers can display model
// info and support runtime profile switching even when the provider is nil.
func NewProvider(projectRoot string) *ProviderResult {
	cfg, resolved := llmconfig.Resolve(projectRoot)

	// Create OAuth store for subscription-based providers.
	var store *oauth.Store
	if storePath, err := oauth.DefaultStorePath(); err != nil {
		slog.Warn("wire: OAuth store unavailable", "err", err)
	} else if store, err = oauth.NewStore(storePath); err != nil {
		slog.Warn("wire: failed to create OAuth store", "err", err)
	}

	// Create key store for TUI-entered API keys.
	var keyStore *oauth.KeyStore
	if keyPath, err := oauth.DefaultKeyStorePath(); err != nil {
		slog.Warn("wire: key store unavailable", "err", err)
	} else if keyStore, err = oauth.NewKeyStore(keyPath); err != nil {
		slog.Warn("wire: failed to create key store", "err", err)
	}

	// If the resolved profile uses OAuth, wire up the authenticator.
	wireOAuth(resolved, store)

	// If no provider yet, check the key store for a stored API key.
	if !resolved.HasProvider() && keyStore != nil {
		WireStoredKey(resolved, keyStore)
	}

	return &ProviderResult{
		Provider:   resolved.NewProvider(),
		Config:     cfg,
		Resolved:   resolved,
		OAuthStore: store,
		KeyStore:   keyStore,
	}
}

// WireOAuthProfile sets up OAuth authentication on a resolved profile
// if it has stored tokens. Returns true if auth was wired.
func WireOAuthProfile(resolved *llmconfig.Resolved, store *oauth.Store) bool {
	return wireOAuth(resolved, store)
}

// WireStoredKey sets the API key on a resolved profile from the key store.
// Returns true if a stored key was found and applied.
func WireStoredKey(resolved *llmconfig.Resolved, keyStore *oauth.KeyStore) bool {
	if resolved == nil || keyStore == nil || resolved.Profile == "" {
		return false
	}
	key := strings.TrimSpace(keyStore.Get(resolved.Profile))
	if key == "" {
		return false
	}
	resolved.SetAPIKey(key)
	return true
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
