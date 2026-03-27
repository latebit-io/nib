package lsp

import (
	"net/url"
	"strings"
)

// pathToURI converts a filesystem path to an LSP file:// URI.
// Uses url.URL to produce correct RFC 3986 encoding (slashes preserved).
func pathToURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

// uriToPath converts an LSP file:// URI to a filesystem path.
// Returns the input unchanged if it doesn't start with file://.
func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return u.Path
}
