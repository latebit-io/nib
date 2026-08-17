// Package oauth implements OAuth authentication flows for LLM providers
// that use subscription-based access (ChatGPT, GitHub Copilot).
//
// It supports two flows:
//   - Device code flow (RFC 8628 + OpenAI variant) for terminal environments
//   - Authorization Code + PKCE for browser-based environments
//
// Tokens are stored on disk and refreshed automatically.
package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// ProviderID identifies an OAuth provider for storage keying and flow selection.
type ProviderID string

const (
	// ProviderOpenAI is the ChatGPT/Codex OAuth provider.
	ProviderOpenAI ProviderID = "openai"
	// ProviderCopilot is the GitHub Copilot OAuth provider.
	ProviderCopilot ProviderID = "copilot"
)

// Token holds OAuth credentials with expiry metadata.
type Token struct {
	// AccessToken is the bearer token used for API requests.
	AccessToken string `json:"access_token"`
	// RefreshToken is used to obtain new access tokens (OpenAI only).
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is when the access token expires. Zero means unknown/never.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// AccountID is the ChatGPT account ID (OpenAI only).
	AccountID string `json:"account_id,omitempty"`
}

// Valid reports whether the token is non-empty and not expired.
// Includes a 30-second buffer to avoid using tokens that are about to expire.
func (t *Token) Valid() bool {
	if t == nil || t.AccessToken == "" {
		return false
	}
	if t.ExpiresAt.IsZero() {
		return true
	}
	return time.Now().Add(30 * time.Second).Before(t.ExpiresAt)
}

// TokenSource provides access tokens, refreshing as needed.
type TokenSource interface {
	// Token returns a valid token, refreshing if the current one is expired.
	Token(ctx context.Context) (*Token, error)
}

// Authenticator sets auth headers on outgoing HTTP requests using an OAuth token source.
type Authenticator struct {
	source TokenSource
}

// NewAuthenticator creates an Authenticator that uses the given TokenSource.
func NewAuthenticator(source TokenSource) *Authenticator {
	return &Authenticator{source: source}
}

// Authenticate sets the Authorization header (and ChatGPT-Account-Id for OpenAI)
// on the given request.
func (a *Authenticator) Authenticate(ctx context.Context, req *http.Request) error {
	tok, err := a.source.Token(ctx)
	if err != nil {
		return err
	}
	if tok == nil || tok.AccessToken == "" {
		return fmt.Errorf("oauth: token source returned empty token")
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if tok.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", tok.AccountID)
	}
	return nil
}

// invalidatableSource is the optional capability a [TokenSource] exposes
// when it can discard its stored credential (e.g. delete a revoked token
// from the backing store).
type invalidatableSource interface {
	Invalidate() error
}

// Invalidate discards the underlying credential when the token source
// supports it, so a server-rejected (revoked) token is not replayed on the
// next run. Satisfies the provider-side invalidation port used after a
// 401. Best-effort: a store-delete failure is logged, not returned — the
// caller has no recovery action, and the in-memory token is gone
// regardless on the next process launch.
//
// No-op when the source cannot invalidate (it stays usable), so callers
// can invoke this unconditionally after an auth failure.
func (a *Authenticator) Invalidate() {
	inv, ok := a.source.(invalidatableSource)
	if !ok {
		return
	}
	if err := inv.Invalidate(); err != nil {
		slog.Warn("oauth: token invalidation failed", "err", err)
	}
}

// DeviceCode holds the response from a device authorization request.
type DeviceCode struct {
	// DeviceCode is the device verification code (sent to the token endpoint).
	DeviceCode string
	// UserCode is the code the user enters in the browser.
	UserCode string
	// VerificationURI is the URL the user visits to enter the code.
	VerificationURI string
	// Interval is the polling interval in seconds.
	Interval int
	// ExpiresIn is how long the device code is valid, in seconds.
	ExpiresIn int
}

// FlowCallbacks lets callers receive progress updates from OAuth flows.
type FlowCallbacks struct {
	// OnBrowserOpen is called when a browser URL is about to be opened.
	OnBrowserOpen func(url string)
	// OnSuccess is called when authentication succeeds.
	OnSuccess func(provider ProviderID)
	// OnError is called when authentication fails.
	OnError func(provider ProviderID, err error)
}
