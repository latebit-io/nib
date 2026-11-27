package mcp

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

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
	client := originClient(endpoint)
	if len(opts.Headers) > 0 {
		client.Transport = &headerTransport{origin: origin(endpoint), headers: opts.Headers}
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

// maxRedirects is how many redirects a request may follow, as net/http does.
const maxRedirects = 10

// originClient returns a client that follows redirects only within the
// origin of address: a token or a configured header sent to a server
// must not reach another host, or the same host over plain http.
func originClient(address string) *http.Client {
	allowed := origin(address)
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if got := origin(req.URL.String()); got != allowed {
			return fmt.Errorf("refusing a redirect from %s to %s: credentials stay with the server", allowed, got)
		}
		return nil
	}}
}

// origin is an address's scheme and host, lowercase; "" if it does not parse.
func origin(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// headerTransport adds a server's configured headers to its own requests.
type headerTransport struct {
	origin  string
	headers map[string]string
}

// RoundTrip implements [http.RoundTripper].
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if origin(req.URL.String()) == t.origin {
		req = req.Clone(req.Context()) // a RoundTripper must not change the caller's request
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}
