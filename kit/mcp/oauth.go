package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/internal/atomicfile"
	"github.com/latebit-io/nib/kit/internal/filecap"
)

// ErrNoLoginNeeded is returned by [Login] for a server that did not ask
// anyone to sign in.
var ErrNoLoginNeeded = errors.New("the server did not ask for a sign-in")

// LoginRequiredError is a server that needs the person to sign in,
// where nobody can be asked: a session only uses sign-ins [Login] kept.
type LoginRequiredError struct{ Server string }

// Error implements error.
func (e *LoginRequiredError) Error() string {
	return fmt.Sprintf("MCP server %s needs you to sign in", e.Server)
}

// maxTokenFileBytes caps the token file.
const maxTokenFileBytes = 1 << 20

// TokenStore keeps sign-ins to HTTP servers, one per server address,
// with what refreshing a token needs. The file is the person's alone.
type TokenStore struct {
	path string
	mu   sync.Mutex
}

// OpenTokenStore returns the store kept at path, created on the first sign-in.
func OpenTokenStore(path string) *TokenStore { return &TokenStore{path: path} }

// DefaultTokenPath is where a binary keeps the store: in the user's
// config directory, which a repository cannot write to.
func DefaultTokenPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("mcp: resolve user config dir: %w", err)
	}
	return filepath.Join(dir, brand.ConfigDirName, "mcp-auth.json"), nil
}

// grant is one sign-in.
type grant struct {
	ClientID     string           `json:"client_id"`
	ClientSecret string           `json:"client_secret,omitempty"`
	TokenURL     string           `json:"token_url"`
	AuthStyle    oauth2.AuthStyle `json:"auth_style,omitempty"`
	// Resource is the audience the token was granted for, sent again on refresh.
	Resource string        `json:"resource"`
	Token    *oauth2.Token `json:"token"`
}

type tokenFile struct {
	Servers map[string]grant `json:"servers"`
}

// read returns the file's content; a missing file is an empty store.
func (s *TokenStore) read() (tokenFile, error) {
	f := tokenFile{Servers: map[string]grant{}}
	if _, err := filecap.ReadJSON(s.path, maxTokenFileBytes, "MCP token store", &f); err != nil {
		return tokenFile{}, fmt.Errorf("mcp: %w", err)
	}
	if f.Servers == nil {
		f.Servers = map[string]grant{}
	}
	return f, nil
}

// load returns the sign-in for a server address, or nil.
func (s *TokenStore) load(endpoint string) (*grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	g, ok := f.Servers[canonical(endpoint)]
	if !ok || g.Token == nil {
		return nil, nil
	}
	return &g, nil
}

// update changes one entry. The file is read again first, so two nib
// processes do not drop each other's sign-ins.
func (s *TokenStore) update(endpoint string, g *grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	if g == nil {
		delete(f.Servers, canonical(endpoint))
	} else {
		f.Servers[canonical(endpoint)] = *g
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("mcp: encode token store: %w", err)
	}
	return atomicfile.Write(s.path, data, 0o700, 0o600)
}

// canonical is a server address as a key: lowercase scheme and host,
// default port, fragment and trailing slash dropped.
func canonical(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
	}
	u.Fragment, u.RawFragment = "", ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// authHandler gives the SDK's HTTP transport the stored token for one
// server. A session cannot sign anyone in: a refused request is a
// [LoginRequiredError]. [Login] uses the SDK's own handler instead.
type authHandler struct {
	server, endpoint string
	store            *TokenStore

	mu     sync.Mutex
	loaded bool // the store was read; a server without a sign-in is not read again
	source oauth2.TokenSource
}

// TokenSource implements [sdkauth.OAuthHandler]. Nil means no token.
func (h *authHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loaded {
		return h.source, nil
	}
	g, err := h.store.load(h.endpoint)
	if err != nil {
		return nil, err
	}
	h.loaded = true
	if g != nil {
		h.source = newGrantSource(h.server, h.endpoint, h.store, *g)
	}
	return h.source, nil
}

// Authorize implements [sdkauth.OAuthHandler]: the server refused the
// request. The store is read again next time: a sign-in may have come since.
func (h *authHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	h.mu.Lock()
	h.loaded, h.source = false, nil
	h.mu.Unlock()
	if err := resp.Body.Close(); err != nil {
		slog.Debug("mcp: close refused response", "err", err)
	}
	return &LoginRequiredError{Server: h.server}
}

// refreshTimeout bounds a token refresh, which gets no context.
const refreshTimeout = 30 * time.Second

// grantSource hands out a sign-in's token, refreshed by oauth2 when it
// expires, and keeps each new one in the store.
type grantSource struct {
	server, endpoint string
	store            *TokenStore
	base             oauth2.TokenSource

	mu    sync.Mutex
	grant grant
}

func newGrantSource(server, endpoint string, store *TokenStore, g grant) *grantSource {
	cfg := &oauth2.Config{ClientID: g.ClientID, ClientSecret: g.ClientSecret,
		Endpoint: oauth2.Endpoint{TokenURL: g.TokenURL, AuthStyle: g.AuthStyle}}
	client := &http.Client{Timeout: refreshTimeout, Transport: resourceParam{resource: g.Resource}}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
	return &grantSource{server: server, endpoint: endpoint, store: store, base: cfg.TokenSource(ctx, g.Token), grant: g}
}

// Token implements [oauth2.TokenSource].
func (s *grantSource) Token() (*oauth2.Token, error) {
	tok, err := s.base.Token()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		retrieve, ok := errors.AsType[*oauth2.RetrieveError](err)
		if s.grant.Token.RefreshToken == "" || (ok && retrieve.ErrorCode == "invalid_grant") {
			return nil, fmt.Errorf("%w: %w", &LoginRequiredError{Server: s.server}, err)
		}
		return nil, err
	}
	if tok.AccessToken != s.grant.Token.AccessToken {
		s.grant.Token = tok
		if err := s.store.update(s.endpoint, &s.grant); err != nil {
			// The token works for this session; a later one signs in again.
			slog.Warn("mcp: keep refreshed token", "server", s.server, "err", err)
		}
	}
	return tok, nil
}

// resourceParam names the granted resource in a refresh, which oauth2
// has no option for: the specification wants it in every token request.
type resourceParam struct{ resource string }

// RoundTrip implements [http.RoundTripper].
func (t resourceParam) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.resource == "" || req.Body == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if closeErr := req.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: refresh: %w", err)
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("mcp: refresh: %w", err)
	}
	form.Set("resource", t.resource)
	encoded := form.Encode()
	req = req.Clone(req.Context())
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader(encoded)), int64(len(encoded))
	return http.DefaultTransport.RoundTrip(req)
}

// LoginOptions say how the person is taken to the sign-in page.
type LoginOptions struct {
	// Open opens an address in the person's browser.
	Open func(url string) error
	// Notify tells the person what is happening, including the address
	// to open by hand when no browser opens.
	Notify func(msg string)
	// Timeout bounds the wait for the person. Zero means five minutes.
	Timeout time.Duration
}

// Login signs the person in to an HTTP server with OAuth in the browser
// and keeps the sign-in in store, replacing an earlier one. A server
// that never asks returns [ErrNoLoginNeeded].
func Login(ctx context.Context, server Server, store *TokenStore, opts LoginOptions) error {
	if server.URL == "" {
		return fmt.Errorf("mcp: %s is not an HTTP server", server.Name)
	}
	if err := store.update(server.URL, nil); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("mcp: listen for the sign-in redirect: %w", err)
	}
	cb := newCallback(ln)
	defer cb.close()

	var resource atomic.Value // what the authorization request named, for refreshing
	var signedIn atomic.Bool
	config := &sdkauth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &sdkauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: brand.Name, RedirectURIs: []string{cb.redirect},
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none",
		}},
		RedirectURL:         cb.redirect,
		RequestRefreshToken: true,
		AuthorizationCodeFetcher: func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			if u, err := url.Parse(args.URL); err == nil {
				resource.Store(u.Query().Get("resource"))
			}
			return cb.wait(ctx, args.URL, opts)
		},
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			res, _ := resource.Load().(string) // always a string: only Store above sets it
			g := grant{
				ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
				TokenURL: cfg.Endpoint.TokenURL, AuthStyle: cfg.Endpoint.AuthStyle,
				Resource: res, Token: tok,
			}
			if err := store.update(server.URL, &g); err != nil {
				return nil, err
			}
			signedIn.Store(true)
			return newGrantSource(server.Name, server.URL, store, g), nil
		},
	}
	if server.ClientID != "" {
		config.PreregisteredClient = &oauthex.ClientCredentials{ClientID: server.ClientID}
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(config)
	if err != nil {
		return fmt.Errorf("mcp: sign-in: %w", err)
	}
	client, err := NewHTTPClient(server.URL, HTTPOptions{Headers: server.Headers, Auth: handler})
	if err != nil {
		return err
	}
	if err := client.Initialize(ctx); err != nil {
		return err
	}
	if err := client.Close(); err != nil {
		slog.Debug("mcp: close after sign-in", "err", err)
	}
	if !signedIn.Load() {
		return ErrNoLoginNeeded
	}
	return nil
}

// callback receives the authorization server's redirect on this machine.
type callback struct {
	redirect string
	server   *http.Server
	results  chan url.Values
}

func newCallback(ln net.Listener) *callback {
	cb := &callback{
		redirect: fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port),
		results:  make(chan url.Values, 1),
	}
	cb.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(cb.serve)}
	go func() {
		if err := cb.server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("mcp: sign-in redirect listener", "err", err)
		}
	}()
	return cb
}

// serve takes one redirect. The page echoes nothing from the request.
func (cb *callback) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		http.NotFound(w, r)
		return
	}
	select {
	case cb.results <- r.URL.Query():
	default: // a redirect is already waiting to be read
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := io.WriteString(w, brand.Name+": you can close this tab and return to the terminal.\n"); err != nil {
		slog.Debug("mcp: sign-in page", "err", err)
	}
}

// wait sends the person to the sign-in page and waits for the redirect.
// The SDK checks state and issuer; an error the server reports is not
// shown, since it may come from a server that is not the issuer.
func (cb *callback) wait(ctx context.Context, address string, opts LoginOptions) (*sdkauth.AuthorizationResult, error) {
	if err := checkEndpoint(address); err != nil {
		return nil, fmt.Errorf("mcp: refusing to open the sign-in page: %w", err)
	}
	notify := func(msg string) {
		if opts.Notify != nil {
			opts.Notify(msg)
		}
	}
	notify("Sign in at: " + address)
	if opts.Open != nil {
		if err := opts.Open(address); err != nil {
			notify("No browser opened (" + err.Error() + "); open the address by hand.")
		}
	}
	timeout := cmp.Or(opts.Timeout, 5*time.Minute)
	select {
	case q := <-cb.results:
		if q.Get("code") == "" {
			return nil, errors.New("mcp: the sign-in did not complete")
		}
		return &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	case <-time.After(timeout):
		return nil, errors.New("mcp: gave up waiting for the sign-in")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (cb *callback) close() {
	if err := cb.server.Close(); err != nil {
		slog.Debug("mcp: close sign-in listener", "err", err)
	}
}
