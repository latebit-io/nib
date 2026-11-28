// Package httporigin keeps credentials with the server they are for:
// endpoints must be https unless on this machine, redirects stay within
// one origin, and configured headers reach only that origin.
package httporigin

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
)

// MaxRedirects is how many redirects a request may follow, as net/http does.
const MaxRedirects = 10

// Of is an address's scheme and host, lowercase, with a default port
// left out (https://h:443 is https://h); "" if it does not parse.
func Of(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" || strings.Contains(host, ":") {
		host = net.JoinHostPort(host, port) // brackets an IPv6 address
		host = strings.TrimSuffix(host, ":")
	}
	return scheme + "://" + host
}

// Page parses a web address: http or https, with a host.
func Page(address string) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("address %q: %w", address, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q is not a web address", address)
	}
	return u, nil
}

// CheckEndpoint refuses an address a token must not be sent to: one
// that is not https, unless it is on this machine.
func CheckEndpoint(address string) error {
	u, err := Page(address)
	if err != nil {
		return err
	}
	if u.Scheme == "http" && !Loopback(u.Hostname()) {
		return fmt.Errorf("%s must be https unless it is on this machine", address)
	}
	return nil
}

// Loopback reports whether host names this machine.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Client returns a client that follows redirects only within the
// origin of address and adds headers to that origin's requests alone:
// a token must not reach another host, or the same host over plain http.
func Client(address string, headers map[string]string) *http.Client {
	allowed := Of(address)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", MaxRedirects)
		}
		if got := Of(req.URL.String()); got != allowed {
			return fmt.Errorf("refusing a redirect from %s to %s: credentials stay with the server", allowed, got)
		}
		return nil
	}}
	if len(headers) > 0 {
		client.Transport = &headerTransport{origin: allowed, headers: headers}
	}
	return client
}

// headerTransport adds a server's configured headers to its own requests.
type headerTransport struct {
	origin  string
	headers map[string]string
}

// RoundTrip implements [http.RoundTripper].
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if Of(req.URL.String()) == t.origin {
		req = req.Clone(req.Context()) // a RoundTripper must not change the caller's request
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// envReference is a ${NAME} in a header value.
var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ReadsEnv returns the first header, in name order, whose value holds a
// ${NAME} reference, or "" when none does.
func ReadsEnv(headers map[string]string) string {
	for _, key := range slices.Sorted(maps.Keys(headers)) {
		if envReference.MatchString(headers[key]) {
			return key
		}
	}
	return ""
}

// ExpandHeaders fills ${NAME} references from the environment. A
// reference to a variable that is not set is an error, naming it.
func ExpandHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(headers))
	for _, key := range slices.Sorted(maps.Keys(headers)) {
		var missing []string
		out[key] = envReference.ReplaceAllStringFunc(headers[key], func(ref string) string {
			name := ref[2 : len(ref)-1] // inside ${...}
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("header %s uses %s, which is not set", key, strings.Join(missing, ", "))
		}
	}
	return out, nil
}
