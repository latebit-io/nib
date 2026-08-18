package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/nib/ai/brand"
)

// OpenAI OAuth constants — matches the Codex CLI client registration.
const (
	openAIClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIAuthURL      = "https://auth.openai.com/oauth/authorize"
	openAITokenURL     = "https://auth.openai.com/oauth/token"
	openAICallbackPort = 1455
)

// openAIScopes are the OAuth scopes requested for ChatGPT access.
var openAIScopes = []string{"openid", "profile", "email", "offline_access"}

// refreshDefaultTTL is the conservative access-token lifetime assumed when a
// refresh response omits expires_in. Short enough to force a prompt
// re-refresh rather than treating the token as eternal.
const refreshDefaultTTL = 5 * time.Minute

// OpenAITokenSource provides tokens for the ChatGPT/Codex API.
// It handles automatic refresh of expired tokens.
type OpenAITokenSource struct {
	store *Store
	mu    sync.Mutex
}

// NewOpenAITokenSource creates a token source backed by the given store.
func NewOpenAITokenSource(store *Store) *OpenAITokenSource {
	return &OpenAITokenSource{store: store}
}

// Token returns a valid access token, refreshing if expired.
func (s *OpenAITokenSource) Token(ctx context.Context) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tok := s.store.Get(ProviderOpenAI)
	if tok.Valid() {
		return tok, nil
	}

	// Token expired or missing — try refresh.
	if tok != nil && tok.RefreshToken != "" {
		refreshed, err := refreshAccessToken(ctx, openAITokenURL, openAIClientID, tok.RefreshToken)
		if err != nil {
			slog.Warn("oauth: OpenAI token refresh failed", "err", err)
			return nil, fmt.Errorf("token refresh failed: %w (re-authenticate with 'connect to chatgpt')", err)
		}
		newTok := &Token{
			AccessToken:  refreshed.AccessToken,
			RefreshToken: refreshed.RefreshToken,
			AccountID:    tok.AccountID,
		}
		if newTok.RefreshToken == "" {
			newTok.RefreshToken = tok.RefreshToken
		}
		if refreshed.ExpiresIn > 0 {
			newTok.ExpiresAt = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
		} else {
			// A refresh response that omits expires_in would leave
			// ExpiresAt zero, which Token.Valid() treats as never
			// expiring — the token would never re-refresh. Apply a
			// conservative default TTL so the next call refreshes.
			newTok.ExpiresAt = time.Now().Add(refreshDefaultTTL)
		}
		// Re-extract account ID from new token if available.
		if id := extractAccountID(refreshed.AccessToken, refreshed.IDToken); id != "" {
			newTok.AccountID = id
		}
		if err := s.store.Put(ProviderOpenAI, newTok); err != nil {
			slog.Warn("oauth: failed to persist refreshed token", "err", err)
		}
		return newTok, nil
	}

	return nil, fmt.Errorf("no valid OpenAI token (authenticate with 'connect to chatgpt')")
}

// Invalidate deletes the stored OpenAI token. Called after the Codex
// endpoint rejects the token with a 401 (revoked or otherwise dead):
// refresh has already been attempted in [OpenAITokenSource.Token], so a
// 401 means even the refreshed token is unusable and the refresh token is
// gone too. Removing it makes [Store.HasToken] report false, so the next
// launch re-offers the connect flow instead of replaying a dead token.
func (s *OpenAITokenSource) Invalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Delete(ProviderOpenAI)
}

// OpenAIBrowserFlow runs the Authorization Code + PKCE flow for OpenAI.
func OpenAIBrowserFlow(ctx context.Context, store *Store, callbacks *FlowCallbacks) error {
	cfg := browserFlowConfig{
		ClientID:     openAIClientID,
		AuthURL:      openAIAuthURL,
		TokenURL:     openAITokenURL,
		RedirectPort: openAICallbackPort,
		RedirectHost: "localhost",
		Scopes:       openAIScopes,
		ExtraParams: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 brand.OAuthOriginator,
		},
	}

	tr, err := browserFlow(ctx, cfg, callbacks)
	if err != nil {
		return err
	}

	tok := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
	}
	if tr.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	tok.AccountID = extractAccountID(tr.AccessToken, tr.IDToken)

	if err := store.Put(ProviderOpenAI, tok); err != nil {
		return fmt.Errorf("store token: %w", err)
	}

	if callbacks != nil && callbacks.OnSuccess != nil {
		callbacks.OnSuccess(ProviderOpenAI)
	}
	return nil
}

// extractAccountID attempts to extract the ChatGPT account ID from JWT tokens.
// Tries the access token first, then the ID token. Returns empty if not found.
func extractAccountID(accessToken, idToken string) string {
	for _, tok := range []string{accessToken, idToken} {
		if tok == "" {
			continue
		}
		if id := extractAccountIDFromJWT(tok); id != "" {
			return id
		}
	}
	return ""
}

// extractAccountIDFromJWT parses a JWT (without validation — we trust the issuer)
// and looks for the ChatGPT account ID in known claim locations.
func extractAccountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}

	// Base64-decode the payload (part 1). JWT uses raw URL encoding.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	// Try known claim locations.
	for _, key := range []string{
		"chatgpt_account_id",
		"https://api.openai.com/auth.chatgpt_account_id",
	} {
		if id, ok := claims[key].(string); ok && id != "" {
			return id
		}
	}

	// Try organizations array — first org's ID.
	if orgs, ok := claims["organizations"].([]any); ok && len(orgs) > 0 {
		if org, ok := orgs[0].(map[string]any); ok {
			if id, ok := org["id"].(string); ok && id != "" {
				return id
			}
		}
	}

	return ""
}
