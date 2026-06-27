package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauthClient is a shared HTTP client with a timeout for OAuth requests.
// Prevents indefinite hangs if the auth server is unresponsive.
var oauthClient = &http.Client{Timeout: 30 * time.Second}

// DeviceFlowConfig holds the endpoint configuration for a device code flow.
type DeviceFlowConfig struct {
	// ClientID is the OAuth client identifier.
	ClientID string
	// DeviceCodeURL is the endpoint to request a device code.
	DeviceCodeURL string
	// TokenURL is the endpoint to exchange codes for tokens.
	TokenURL string
	// Scopes are the OAuth scopes to request.
	Scopes []string
	// GrantType is the grant type for the token request.
	// Standard RFC 8628: "urn:ietf:params:oauth:grant-type:device_code"
	GrantType string
}

// deviceCodeResponse is the JSON response from a device code request (RFC 8628).
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// tokenResponse is the JSON response from a token exchange.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// RequestDeviceCode initiates a standard RFC 8628 device code flow.
// Returns the device code info that should be shown to the user.
func RequestDeviceCode(ctx context.Context, cfg DeviceFlowConfig) (*DeviceCode, *deviceCodeResponse, error) {
	data := url.Values{
		"client_id": {cfg.ClientID},
	}
	if len(cfg.Scopes) > 0 {
		data.Set("scope", strings.Join(cfg.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.DeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("create device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("device code request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body already read; close error is not actionable

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return nil, nil, fmt.Errorf("read device code response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("device code request: HTTP %d", resp.StatusCode)
	}

	var dcr deviceCodeResponse
	if err := json.Unmarshal(body, &dcr); err != nil {
		return nil, nil, fmt.Errorf("decode device code response: %w", err)
	}

	interval := dcr.Interval
	if interval < 5 {
		interval = 5
	}

	dc := &DeviceCode{
		DeviceCode:      dcr.DeviceCode,
		UserCode:        dcr.UserCode,
		VerificationURI: dcr.VerificationURI,
		Interval:        interval,
		ExpiresIn:       dcr.ExpiresIn,
	}
	return dc, &dcr, nil
}

// pollResult classifies a single token poll response.
type pollResult int

const (
	pollSuccess pollResult = iota
	pollPending
	pollSlowDown
)

// pollOnce makes a single token request and classifies the response.
func pollOnce(ctx context.Context, cfg DeviceFlowConfig, dc *DeviceCode) (*tokenResponse, pollResult, error) {
	data := url.Values{
		"client_id":   {cfg.ClientID},
		"device_code": {dc.DeviceCode},
		"grant_type":  {cfg.GrantType},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, 0, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("token request: %w", err)
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8192))
	_ = resp.Body.Close() // body already read; close error is not actionable
	if readErr != nil {
		return nil, 0, fmt.Errorf("read token response: %w", readErr)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, 0, fmt.Errorf("decode token response: %w", err)
	}

	switch tr.Error {
	case "":
		return &tr, pollSuccess, nil
	case "authorization_pending":
		return nil, pollPending, nil
	case "slow_down":
		return nil, pollSlowDown, nil
	case "expired_token":
		return nil, 0, fmt.Errorf("device code expired")
	case "access_denied":
		return nil, 0, fmt.Errorf("access denied by user")
	default:
		desc := tr.ErrorDesc
		if desc == "" {
			desc = tr.Error
		}
		return nil, 0, fmt.Errorf("token error: %s", desc)
	}
}

// PollDeviceToken polls the token endpoint until the user authorizes,
// the context is cancelled, or the device code expires.
func PollDeviceToken(ctx context.Context, cfg DeviceFlowConfig, dc *DeviceCode) (*tokenResponse, error) {
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	interval := time.Duration(dc.Interval) * time.Second
	// Reuse one timer instead of time.After per iteration, which would
	// leak a timer until fire on ctx-cancel.
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device code expired")
		}

		tr, result, err := pollOnce(ctx, cfg, dc)
		if err != nil {
			return nil, err
		}
		switch result {
		case pollSuccess:
			return tr, nil
		case pollSlowDown:
			interval += 5 * time.Second
		case pollPending:
			// continue polling
		}
		// Channel drained by the select above, so Reset is safe. interval
		// may have grown on a slow_down response.
		timer.Reset(interval)
	}
}

// RefreshAccessToken exchanges a refresh token for a new access token.
func RefreshAccessToken(ctx context.Context, tokenURL, clientID, refreshToken string) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body already read; close error is not actionable

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh: HTTP %d", resp.StatusCode)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.Error != "" {
		desc := tr.ErrorDesc
		if desc == "" {
			desc = tr.Error
		}
		return nil, fmt.Errorf("refresh error: %s", desc)
	}
	return &tr, nil
}
