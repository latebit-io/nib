package llmconfig

import (
	"context"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/oauth"
)

// WireOAuth attaches an OAuth authenticator to a resolved profile when
// the profile uses subscription-based auth and the store holds a valid or
// refreshable token for the provider. Returns true if Auth was set.
//
// This is the canonical OAuth-attach for any binary that resolves an
// LLM profile. Each binary owns its own store creation (default path or
// otherwise) and passes the store in. Centralizing here means one
// switch on [oauth.ProviderID]; new OAuth providers add a single case.
func WireOAuth(resolved *Resolved, store *oauth.Store) bool {
	if resolved == nil || store == nil || resolved.OAuthProvider == "" {
		return false
	}
	// Validate the provider before probing the store. An unknown provider
	// must surface as a config error (warn-and-return), not silently
	// collapse into the same "no token on disk" path as ProviderOpenAI
	// without a token — callers cannot distinguish the two from the bool
	// alone, and the warning is the only signal a misconfigured profile
	// gets.
	providerID := oauth.ProviderID(resolved.OAuthProvider)
	var tokenSource oauth.TokenSource
	switch providerID {
	case oauth.ProviderOpenAI:
		tokenSource = oauth.NewOpenAITokenSource(store)
	case oauth.ProviderCopilot:
		tokenSource = oauth.NewCopilotTokenSource(store)
	default:
		slog.Warn("llmconfig: unknown OAuth provider", "provider", resolved.OAuthProvider)
		return false
	}
	// An expired token with a refresh token stays wired: the TokenSource
	// refreshes on first use instead of forcing a fresh browser sign-in.
	if !store.Get(providerID).Usable() {
		return false
	}
	resolved.Auth = oauth.NewAuthenticator(tokenSource)
	return true
}

// CheckAuth verifies the wired authenticator can mint credentials now,
// refreshing an expired OAuth token. Returns nil when no authenticator is
// wired or it cannot check itself.
func (r *Resolved) CheckAuth(ctx context.Context) error {
	if r == nil || r.Auth == nil {
		return nil
	}
	if cc, ok := r.Auth.(llm.CredentialChecker); ok {
		return cc.Ready(ctx)
	}
	return nil
}

// WireStoredKey applies an API key from the key store to a resolved
// profile, keyed by profile name. Returns true only when a key was
// actually applied — OAuth-only profiles short-circuit with false
// because [Resolved.SetAPIKey] is a no-op for them, and a true return
// in that case would mislead callers into believing credentials are
// wired when they aren't.
func WireStoredKey(resolved *Resolved, keyStore *oauth.KeyStore) bool {
	if resolved == nil || keyStore == nil || resolved.Profile == "" {
		return false
	}
	if resolved.OAuthProvider != "" {
		return false
	}
	key := strings.TrimSpace(keyStore.Get(resolved.Profile))
	if key == "" {
		return false
	}
	resolved.SetAPIKey(key)
	return true
}
