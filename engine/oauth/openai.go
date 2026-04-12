package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OpenAI OAuth constants — matches the Codex CLI client registration.
const (
	openAIClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIAuthURL       = "https://auth.openai.com/oauth/authorize"
	openAITokenURL      = "https://auth.openai.com/oauth/token"
	openAIDeviceURL     = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	openAIDevicePollURL = "https://auth.openai.com/api/accounts/deviceauth/token"
	openAIDeviceVerify  = "https://auth.openai.com/codex/device"
	openAICallbackPort  = 1455
)

// openAIScopes are the OAuth scopes requested for ChatGPT access.
var openAIScopes = []string{"openid", "profile", "email", "offline_access"}

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
		refreshed, err := RefreshAccessToken(ctx, openAITokenURL, openAIClientID, tok.RefreshToken)
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

// OpenAIDeviceFlow runs OpenAI's custom device code flow.
// OpenAI uses a non-standard flow: request a user code, poll a custom endpoint,
// then exchange the result for a standard OAuth token.
func OpenAIDeviceFlow(ctx context.Context, store *Store, callbacks *FlowCallbacks) error {
	// Step 1: Request device code.
	resp, err := openAIRequestDeviceCode(ctx)
	if err != nil {
		return fmt.Errorf("request device code: %w", err)
	}

	dc := &DeviceCode{
		DeviceCode:      resp.deviceAuthID,
		UserCode:        resp.userCode,
		VerificationURI: openAIDeviceVerify,
		Interval:        5,
		ExpiresIn:       900, // 15 minutes
	}
	if callbacks != nil && callbacks.OnDeviceCode != nil {
		callbacks.OnDeviceCode(*dc)
	}

	// Step 2: Poll for authorization.
	authResult, err := openAIPollDeviceAuth(ctx, resp.deviceAuthID, resp.userCode, dc.Interval, dc.ExpiresIn)
	if err != nil {
		return fmt.Errorf("device auth poll: %w", err)
	}

	// Step 3: Exchange the authorization code for tokens.
	tr, err := openAIExchangeDeviceCode(ctx, authResult.authCode, authResult.codeVerifier)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
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

// OpenAIBrowserFlow runs the Authorization Code + PKCE flow for OpenAI.
func OpenAIBrowserFlow(ctx context.Context, store *Store, callbacks *FlowCallbacks) error {
	cfg := BrowserFlowConfig{
		ClientID:     openAIClientID,
		AuthURL:      openAIAuthURL,
		TokenURL:     openAITokenURL,
		RedirectPort: openAICallbackPort,
		Scopes:       openAIScopes,
		ExtraParams: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "junto",
		},
	}

	tr, err := BrowserFlow(ctx, cfg, callbacks)
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

// --- OpenAI custom device auth internals ---

type openAIDeviceCodeResp struct {
	deviceAuthID string
	userCode     string
}

type openAIAuthResult struct {
	authCode     string
	codeVerifier string
}

func openAIRequestDeviceCode(ctx context.Context) (*openAIDeviceCodeResp, error) {
	data := url.Values{
		"client_id": {openAIClientID},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", openAIDeviceURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &openAIDeviceCodeResp{
		deviceAuthID: result.DeviceAuthID,
		userCode:     result.UserCode,
	}, nil
}

func openAIPollDeviceAuth(ctx context.Context, deviceAuthID, userCode string, intervalSec, expiresInSec int) (*openAIAuthResult, error) {
	deadline := time.Now().Add(time.Duration(expiresInSec) * time.Second)
	interval := time.Duration(intervalSec) * time.Second

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device code expired")
		}

		data := url.Values{
			"device_auth_id": {deviceAuthID},
			"user_code":      {userCode},
		}

		req, err := http.NewRequestWithContext(ctx, "POST", openAIDevicePollURL, strings.NewReader(data.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := oauthClient.Do(req)
		if err != nil {
			return nil, err
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close() // body already read; close error is not actionable
		if readErr != nil {
			return nil, readErr
		}

		var result struct {
			Status       string `json:"status"`
			AuthCode     string `json:"authorization_code"`
			CodeVerifier string `json:"code_verifier"`
			Error        string `json:"error"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decode poll response: %w", err)
		}

		switch {
		case result.AuthCode != "":
			return &openAIAuthResult{
				authCode:     result.AuthCode,
				codeVerifier: result.CodeVerifier,
			}, nil
		case result.Error != "":
			return nil, fmt.Errorf("device auth error: %s", result.Error)
		case result.Status == "expired":
			return nil, fmt.Errorf("device code expired")
		}
		// Still pending — continue polling.
	}
}

func openAIExchangeDeviceCode(ctx context.Context, authCode, codeVerifier string) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {"https://auth.openai.com/deviceauth/callback"},
		"client_id":     {openAIClientID},
		"code_verifier": {codeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", openAITokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		return nil, fmt.Errorf("token exchange: HTTP %d", resp.StatusCode)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.Error != "" {
		desc := tr.ErrorDesc
		if desc == "" {
			desc = tr.Error
		}
		return nil, fmt.Errorf("token error: %s", desc)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token exchange returned empty access token")
	}
	return &tr, nil
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
