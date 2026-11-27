package mcp

import (
	"fmt"
	"net"
	"net/http"
	"net/url"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
	if err := checkEndpoint(endpoint); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	client := &http.Client{}
	if len(opts.Headers) > 0 {
		client.Transport = &headerTransport{headers: opts.Headers}
	}
	return &Client{transport: func() sdk.Transport {
		return &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client, OAuthHandler: opts.Auth}
	}}, nil
}

// checkEndpoint refuses an address a token must not be sent to: one
// that is not https, unless it is on this machine.
func checkEndpoint(address string) error {
	u, err := page(address)
	if err != nil {
		return err
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("%s must be https unless it is on this machine", address)
	}
	return nil
}

// checkPage refuses what is not a web address.
func checkPage(address string) error {
	_, err := page(address)
	return err
}

// page parses a web address: http or https, with a host.
func page(address string) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("address %q: %w", address, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q is not a web address", address)
	}
	return u, nil
}

// loopback reports whether host names this machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// headerTransport adds a server's configured headers to each request.
type headerTransport struct {
	headers map[string]string
}

// RoundTrip implements [http.RoundTripper].
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context()) // a RoundTripper must not change the caller's request
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}
