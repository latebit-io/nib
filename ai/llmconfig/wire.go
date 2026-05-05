package llmconfig

import (
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/oauth"
)

// WireOAuth attaches an OAuth authenticator to a resolved profile when
// the profile uses subscription-based auth and the store holds a token
// for the provider. Returns true if Auth was set.
//
// This is the canonical OAuth-attach for any binary that resolves an
// LLM profile. Each binary owns its own store creation (default path or
// otherwise) and passes the store in. Centralizing here means one
// switch on [oauth.ProviderID]; new OAuth providers add a single case.
func WireOAuth(resolved *Resolved, store *oauth.Store) bool {
	if resolved == nil || store == nil || resolved.OAuthProvider == "" {
		return false
	}
	providerID := oauth.ProviderID(resolved.OAuthProvider)
	if !store.HasToken(providerID) {
		return false
	}
	switch providerID {
	case oauth.ProviderOpenAI:
		resolved.Auth = oauth.NewAuthenticator(oauth.NewOpenAITokenSource(store))
		return true
	case oauth.ProviderCopilot:
		resolved.Auth = oauth.NewAuthenticator(oauth.NewCopilotTokenSource(store))
		return true
	default:
		slog.Warn("llmconfig: unknown OAuth provider", "provider", resolved.OAuthProvider)
		return false
	}
}

// WireStoredKey applies an API key from the key store to a resolved
// profile, keyed by profile name. Returns true if a key was found and
// applied. A no-op for OAuth-only profiles (see [Resolved.SetAPIKey]).
func WireStoredKey(resolved *Resolved, keyStore *oauth.KeyStore) bool {
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
