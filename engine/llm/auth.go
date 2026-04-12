package llm

import (
	"context"
	"net/http"
)

// Auth sets authentication headers on outgoing HTTP requests to LLM providers.
// Implementations handle both static API keys and dynamic OAuth tokens.
type Auth interface {
	// Authenticate adds auth headers to the request. The context allows
	// token refresh operations to be cancelled.
	Authenticate(ctx context.Context, req *http.Request) error
}

// StaticKeyAuth implements Auth with a fixed API key.
type StaticKeyAuth string

// Authenticate sets the Authorization: Bearer header with the static key.
// Skips the header if the key is empty.
func (k StaticKeyAuth) Authenticate(_ context.Context, req *http.Request) error {
	if k != "" {
		req.Header.Set("Authorization", "Bearer "+string(k))
	}
	return nil
}
