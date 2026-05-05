// Package wire provides shared composition logic used by both the TUI
// and headless agent binaries. It wires together LLM providers, memory
// servers, LSP, and MCP tools from environment variables and project config.
package wire

import (
	"log/slog"

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
	llmconfig.WireOAuth(resolved, store)

	// If no provider yet, check the key store for a stored API key.
	if !resolved.HasProvider() && keyStore != nil {
		llmconfig.WireStoredKey(resolved, keyStore)
	}

	return &ProviderResult{
		Provider:   resolved.NewProvider(),
		Config:     cfg,
		Resolved:   resolved,
		OAuthStore: store,
		KeyStore:   keyStore,
	}
}
