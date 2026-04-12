package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// GitHub Copilot OAuth constants.
const (
	githubClientID      = "Ov23li8tweQw6odWQebz"
	githubDeviceCodeURL = "https://github.com/login/device/code"
	githubTokenURL      = "https://github.com/login/oauth/access_token"
	githubScope         = "read:user"
	copilotTokenURL     = "https://api.github.com/copilot_internal/v2/token"
)

// CopilotTokenSource provides tokens for the GitHub Copilot API.
// It stores the long-lived GitHub OAuth token and exchanges it for
// short-lived Copilot session tokens as needed.
type CopilotTokenSource struct {
	store *Store
	mu    sync.Mutex

	// Cached Copilot session token (short-lived, ~30 min).
	sessionToken   string
	sessionExpires time.Time
}

// NewCopilotTokenSource creates a token source backed by the given store.
func NewCopilotTokenSource(store *Store) *CopilotTokenSource {
	return &CopilotTokenSource{store: store}
}

// Token returns a valid Copilot session token, exchanging the stored GitHub
// token for a new session token if the current one is expired.
func (s *CopilotTokenSource) Token(ctx context.Context) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check cached session token.
	if s.sessionToken != "" && time.Now().Add(30*time.Second).Before(s.sessionExpires) {
		return &Token{AccessToken: s.sessionToken, ExpiresAt: s.sessionExpires}, nil
	}

	// Get the stored GitHub OAuth token.
	ghToken := s.store.Get(ProviderCopilot)
	if ghToken == nil || ghToken.AccessToken == "" {
		return nil, fmt.Errorf("no GitHub token (authenticate with 'connect to copilot')")
	}

	// Exchange GitHub token for Copilot session token.
	session, err := exchangeCopilotToken(ctx, ghToken.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("copilot token exchange: %w", err)
	}

	s.sessionToken = session.token
	s.sessionExpires = session.expiresAt

	return &Token{AccessToken: session.token, ExpiresAt: session.expiresAt}, nil
}

// RequestCopilotDeviceCode initiates the GitHub device code flow and returns
// the device code for display to the user. Call CompleteCopilotDeviceFlow
// to poll for authorization and store the token.
func RequestCopilotDeviceCode(ctx context.Context) (*DeviceCode, error) {
	cfg := copilotDeviceFlowConfig()
	dc, _, err := RequestDeviceCode(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	return dc, nil
}

// CompleteCopilotDeviceFlow polls for the user to authorize the device code,
// exchanges the token, verifies Copilot access, and stores the result.
func CompleteCopilotDeviceFlow(ctx context.Context, store *Store, dc *DeviceCode) error {
	cfg := copilotDeviceFlowConfig()

	tr, err := PollDeviceToken(ctx, cfg, dc)
	if err != nil {
		return fmt.Errorf("device code poll: %w", err)
	}

	// Verify the token works with Copilot before persisting.
	// If the account has no Copilot access, we don't want a stored token
	// that makes the profile appear connected while every request fails.
	if _, err := exchangeCopilotToken(ctx, tr.AccessToken); err != nil {
		slog.Warn("oauth: GitHub auth succeeded but Copilot token exchange failed", "err", err)
		return fmt.Errorf("GitHub auth succeeded but Copilot access failed: %w (do you have a Copilot subscription?)", err)
	}

	tok := &Token{
		AccessToken: tr.AccessToken,
		// GitHub tokens don't typically expire, but set a far-future expiry.
		ExpiresAt: time.Now().Add(365 * 24 * time.Hour),
	}
	if err := store.Put(ProviderCopilot, tok); err != nil {
		return fmt.Errorf("store token: %w", err)
	}

	return nil
}

// CopilotDeviceFlow runs the full GitHub device code flow end-to-end.
// For TUI use, prefer RequestCopilotDeviceCode + CompleteCopilotDeviceFlow
// to show the code to the user before polling.
func CopilotDeviceFlow(ctx context.Context, store *Store, callbacks *FlowCallbacks) error {
	dc, err := RequestCopilotDeviceCode(ctx)
	if err != nil {
		return err
	}

	if callbacks != nil && callbacks.OnDeviceCode != nil {
		callbacks.OnDeviceCode(*dc)
	}

	if err := CompleteCopilotDeviceFlow(ctx, store, dc); err != nil {
		return err
	}

	if callbacks != nil && callbacks.OnSuccess != nil {
		callbacks.OnSuccess(ProviderCopilot)
	}
	return nil
}

// copilotDeviceFlowConfig returns the device flow config for GitHub Copilot.
func copilotDeviceFlowConfig() DeviceFlowConfig {
	return DeviceFlowConfig{
		ClientID:      githubClientID,
		DeviceCodeURL: githubDeviceCodeURL,
		TokenURL:      githubTokenURL,
		Scopes:        []string{githubScope},
		GrantType:     "urn:ietf:params:oauth:grant-type:device_code",
	}
}

// copilotSession holds a short-lived Copilot API session token.
type copilotSession struct {
	token     string
	expiresAt time.Time
}

// exchangeCopilotToken exchanges a GitHub OAuth token for a Copilot session token.
func exchangeCopilotToken(ctx context.Context, githubToken string) (*copilotSession, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", copilotTokenURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/json")

	resp, err := oauthClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // body already read; close error is not actionable

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if result.Token == "" {
		return nil, fmt.Errorf("empty token in response")
	}

	return &copilotSession{
		token:     result.Token,
		expiresAt: time.Unix(result.ExpiresAt, 0),
	}, nil
}
