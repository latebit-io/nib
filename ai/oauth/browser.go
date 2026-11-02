package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/nib/ai/brand"
)

// browserFlowConfig holds the endpoint configuration for Authorization Code + PKCE.
type browserFlowConfig struct {
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
	// CallbackPath is the HTTP path for the local callback server.
	// Defaults to "/auth/callback" if empty.
	CallbackPath string
	// RedirectHost is the hostname used in the redirect_uri and for binding
	// the local callback server. Defaults to "127.0.0.1" if empty.
	// Must match the host registered with the OAuth provider.
	RedirectHost string
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

// callbackShutdownTimeout bounds how long close waits for the callback
// handler to finish rendering the exchange result to the browser.
const callbackShutdownTimeout = 5 * time.Second

// errSignInEnded is shown to a callback still waiting when the flow ends
// without exchanging its code.
var errSignInEnded = errors.New("sign-in ended before the code was exchanged")

// callbackServer manages a local HTTP server that receives the OAuth callback.
type callbackServer struct {
	srv         *http.Server
	redirectURI string
	codeCh      chan string
	errCh       chan error
	// resultCh carries the token-exchange outcome back to the handler that
	// delivered the code, so the browser page reports what actually happened.
	resultCh chan error
}

// startCallbackServer binds a local listener and starts serving the callback handler.
// Returns the server (for shutdown) and the redirect URI with the actual bound port.
// A non-zero port is never swapped for another: providers match redirect_uri
// exactly, so a fallback port only fails later at the provider.
func startCallbackServer(host string, port int, state, callbackPath string) (*callbackServer, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen for OAuth callback on port %d (another sign-in may be in progress): %w", port, err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	resultCh := make(chan error, 1)

	// trySendCode and trySendErr are non-blocking sends that drop duplicates.
	// The channels are buffered at 1; a second callback (browser retry) or
	// a server error racing with a successful callback must not block.
	trySendCode := func(code string) bool {
		select {
		case codeCh <- code:
			return true
		default:
			return false
		}
	}
	trySendErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("oauth: callback received", "has_code", r.URL.Query().Has("code"), "error", r.URL.Query().Get("error"))
		if r.URL.Query().Get("state") != state {
			slog.Warn("oauth: callback state mismatch")
			trySendErr(fmt.Errorf("oauth callback: state mismatch (possible CSRF)"))
			writePage(w, "Authentication failed", "State mismatch.")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error")
			if errMsg == "" {
				errMsg = "no code in callback"
			}
			trySendErr(fmt.Errorf("oauth callback: %s", errMsg))
			writePage(w, "Authentication failed", errMsg+" You can close this tab.")
			return
		}
		if !trySendCode(code) {
			writePage(w, "Already received", "You can close this tab.")
			return
		}
		// Hold the response until the code exchange finishes so the page
		// never claims success for a sign-in that failed.
		var exchangeErr error
		select {
		case exchangeErr = <-resultCh:
		case <-r.Context().Done():
			return
		}
		if exchangeErr != nil {
			writePage(w, "Authentication failed", exchangeErr.Error()+" You can close this tab.")
			return
		}
		writePage(w, "Authentication successful!", "You can close this tab and return to "+brand.Name+".")
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			trySendErr(fmt.Errorf("oauth callback server: %w", err))
		}
	}()

	redirectURI := fmt.Sprintf("http://%s%s", net.JoinHostPort(host, strconv.Itoa(actualPort)), callbackPath)
	slog.Debug("oauth: callback server listening", "addr", listener.Addr().String(), "redirect_uri", redirectURI)
	return &callbackServer{
		srv:         srv,
		redirectURI: redirectURI,
		codeCh:      codeCh,
		errCh:       errCh,
		resultCh:    resultCh,
	}, nil
}

// writePage renders a minimal result page; both strings are escaped.
func writePage(w http.ResponseWriter, title, msg string) {
	_, _ = fmt.Fprintf(w, "<html><body><h1>%s</h1><p>%s</p></body></html>", html.EscapeString(title), html.EscapeString(msg)) // client may have disconnected; not actionable
}

// close shuts the server down gracefully so a handler still writing the
// result page finishes, then force-closes anything left.
func (s *callbackServer) close() {
	// A handler can be waiting for an exchange that never ran (flow ended
	// on error or cancel); feed it so Shutdown does not wait out the timeout.
	select {
	case s.resultCh <- errSignInEnded:
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		slog.Debug("oauth: callback server shutdown", "err", err)
		_ = s.srv.Close() // already logged above; nothing further to do
	}
}

// resolvedCallback returns the callback path and redirect host with defaults applied.
func (cfg *browserFlowConfig) resolvedCallback() (path, host string, err error) {
	path = cfg.CallbackPath
	if path == "" {
		path = "/auth/callback"
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return "", "", fmt.Errorf("invalid callback path %q", path)
	}
	host = cfg.RedirectHost
	if host == "" {
		host = "127.0.0.1"
	}
	return path, host, nil
}

// browserFlow runs the Authorization Code + PKCE flow.
// It starts a local HTTP server, opens the browser, waits for the callback,
// and exchanges the auth code for tokens.
func browserFlow(ctx context.Context, cfg browserFlowConfig, callbacks *FlowCallbacks) (*tokenResponse, error) {
	p, err := newPKCE()
	if err != nil {
		return nil, err
	}

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	callbackPath, host, err := cfg.resolvedCallback()
	if err != nil {
		return nil, err
	}
	cb, err := startCallbackServer(host, cfg.RedirectPort, state, callbackPath)
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
		if params.Has(k) {
			continue // do not overwrite security-critical params (state, redirect_uri, etc.)
		}
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

	// Exchange auth code for tokens; the waiting callback handler renders
	// the outcome (resultCh is buffered, so this never blocks).
	tr, err := exchangeCode(ctx, cfg.TokenURL, cfg.ClientID, code, cb.redirectURI, p.verifier)
	cb.resultCh <- err
	if err != nil {
		slog.Warn("oauth: code exchange failed", "err", err)
		return nil, err
	}
	slog.Debug("oauth: code exchange succeeded")
	return tr, nil
}

// exchangeCode exchanges an authorization code for tokens.
func exchangeCode(ctx context.Context, tokenURL, clientID, code, redirectURI, codeVerifier string) (*tokenResponse, error) {
	return postTokenForm(ctx, tokenURL, "token exchange", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
	})
}

// openBrowser opens a URL in the user's default browser.
func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "linux":
		return exec.Command("xdg-open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}
