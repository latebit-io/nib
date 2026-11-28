package mcp

import (
	"fmt"
	"net/http"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/latebit-io/nib/kit/internal/httporigin"
)

// HTTPOptions configure a Streamable HTTP server connection.
type HTTPOptions struct {
	// Headers are sent with every request, such as a static token.
	Headers map[string]string
	// Auth signs requests in, for a server that asks for OAuth.
	Auth sdkauth.OAuthHandler
}

// NewHTTPClient prepares a connection to a Streamable HTTP server at
// endpoint, made by [Client.Initialize]. The endpoint must be https,
// or http to this machine: anything else would send tokens in the clear.
func NewHTTPClient(endpoint string, opts HTTPOptions) (*Client, error) {
	if err := httporigin.CheckEndpoint(endpoint); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	client := httporigin.Client(endpoint, opts.Headers)
	return &Client{transport: func() sdk.Transport {
		return &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client, OAuthHandler: opts.Auth}
	}}, nil
}

// signInClient is the client for sign-in, which talks to the server and
// its authorization server. Every hop must be https (or this machine),
// and a request with a body (the code and PKCE verifier) keeps its origin.
func signInClient() *http.Client {
	return &http.Client{Timeout: refreshTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= httporigin.MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", httporigin.MaxRedirects)
		}
		if err := httporigin.CheckEndpoint(req.URL.String()); err != nil {
			return fmt.Errorf("refusing a redirect: %w", err)
		}
		if from := httporigin.Of(via[0].URL.String()); req.Method != http.MethodGet && httporigin.Of(req.URL.String()) != from {
			return fmt.Errorf("refusing to send a %s from %s on to another origin", req.Method, from)
		}
		return nil
	}}
}
