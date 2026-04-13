package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

// BrowserFlowConfig holds the endpoint configuration for Authorization Code + PKCE.
type BrowserFlowConfig struct {
	// ClientID is the OAuth client identifier.
	ClientID string
	// AuthURL is the authorization endpoint.
	AuthURL string
	// TokenURL is the token exchange endpoint.
	TokenURL string
	// RedirectPort is the local port for the callback server.
	RedirectPort int
	// Scopes are the OAuth scopes to request.
	Scopes []string
	// ExtraParams are additional query parameters for the authorization URL.
	ExtraParams map[string]string
}

// pkce holds a PKCE code verifier and its S256 challenge.
type pkce struct {
	verifier  string
	challenge string
}

// randomState generates a cryptographically random state string for CSRF protection.
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newPKCE generates a random PKCE verifier and its S256 challenge.
func newPKCE() (*pkce, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	return &pkce{verifier: verifier, challenge: challenge}, nil
}

// BrowserFlow runs the Authorization Code + PKCE flow.
// It starts a local HTTP server, opens the browser, waits for the callback,
// and exchanges the auth code for tokens.
// callbackServer manages a local HTTP server that receives the OAuth callback.
type callbackServer struct {
	srv         *http.Server
	redirectURI string
	codeCh      chan string
	errCh       chan error
}

// startCallbackServer binds a local listener and starts serving the callback handler.
// Returns the server (for shutdown) and the redirect URI with the actual bound port.
func startCallbackServer(port int, state string) (*callbackServer, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen for OAuth callback: %w", err)
		}
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- fmt.Errorf("oauth callback: state mismatch (possible CSRF)")
			_, _ = fmt.Fprint(w, "<html><body><h1>Authentication failed</h1><p>State mismatch.</p></body></html>")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error")
			if errMsg == "" {
				errMsg = "no code in callback"
			}
			errCh <- fmt.Errorf("oauth callback: %s", errMsg)
			_, _ = fmt.Fprintf(w, "<html><body><h1>Authentication failed</h1><p>%s</p><p>You can close this tab.</p></body></html>", html.EscapeString(errMsg))
			return
		}
		codeCh <- code
		_, _ = fmt.Fprint(w, "<html><body><h1>Authentication successful!</h1><p>You can close this tab and return to Junto.</p></body></html>")
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("oauth callback server: %w", err)
		}
	}()

	return &callbackServer{
		srv:         srv,
		redirectURI: fmt.Sprintf("http://127.0.0.1:%d/auth/callback", actualPort),
		codeCh:      codeCh,
		errCh:       errCh,
	}, nil
}

func (s *callbackServer) close() { _ = s.srv.Close() }

func BrowserFlow(ctx context.Context, cfg BrowserFlowConfig, callbacks *FlowCallbacks) (*tokenResponse, error) {
	p, err := newPKCE()
	if err != nil {
		return nil, err
	}

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	cb, err := startCallbackServer(cfg.RedirectPort, state)
	if err != nil {
		return nil, err
	}
	defer cb.close()

	// Build authorization URL with the actual redirect URI.
	params := url.Values{
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {cb.redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(cfg.Scopes, " ")},
		"code_challenge":        {p.challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	for k, v := range cfg.ExtraParams {
		params.Set(k, v)
	}
	authURL := cfg.AuthURL + "?" + params.Encode()

	if callbacks != nil && callbacks.OnBrowserOpen != nil {
		callbacks.OnBrowserOpen(authURL)
	}
	if err := openBrowser(authURL); err != nil {
		return nil, fmt.Errorf("open browser: %w (URL: %s)", err, authURL)
	}

	// Wait for callback or context cancellation.
	var code string
	select {
	case code = <-cb.codeCh:
	case err := <-cb.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Exchange auth code for tokens.
	return exchangeCode(ctx, cfg.TokenURL, cfg.ClientID, code, cb.redirectURI, p.verifier)
}

// exchangeCode exchanges an authorization code for tokens.
func exchangeCode(ctx context.Context, tokenURL, clientID, code, redirectURI, codeVerifier string) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body already read; close error is not actionable

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange: HTTP %d", resp.StatusCode)
	}
	return &tr, nil
}

// openBrowser opens a URL in the user's default browser.
func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "linux":
		return exec.Command("xdg-open", u).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", u).Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}
