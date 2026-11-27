package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CodexAPI implements Provider for the ChatGPT/Codex subscription
// endpoint. It speaks the Responses wire format, as [ResponsesAPI] does,
// with the ChatGPT backend's endpoint, model list and defaults.
type CodexAPI struct{ *responses }

var _ OutputCapEscalator = (*CodexAPI)(nil)

// codexProviderName labels errors and [AuthError.Provider].
const codexProviderName = "chatgpt"

// NewCodexAPI creates a CodexAPI provider for the ChatGPT Codex endpoint.
// effort sets the reasoning effort ("" for the provider default).
func NewCodexAPI(model string, auth Auth, effort Effort) *CodexAPI {
	r := newResponses(codexEndpoint, codexProviderName, model, auth, effort)
	// The ChatGPT backend refuses a request without instructions.
	r.instructions = "You are a helpful coding assistant."
	return &CodexAPI{r}
}

// codexClientVersion is the Codex client version this adapter speaks; the
// models endpoint lists only models whose minimal_client_version is at or
// below it (0.1.0 lists nothing). Bump only alongside protocol support for
// that client, or Stream may meet models it cannot drive.
const codexClientVersion = "0.200.0"

// Codex API endpoints.
const (
	codexEndpoint       = "https://chatgpt.com/backend-api/codex/responses"
	codexModelsEndpoint = "https://chatgpt.com/backend-api/codex/models?client_version=" + codexClientVersion
)

// ListModels queries the Codex models endpoint for available models.
// Implements ModelLister.
func (c *CodexAPI) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexModelsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("codex: create models request: %w", err)
	}
	if err := c.auth.Authenticate(ctx, req); err != nil {
		return nil, fmt.Errorf("codex: authenticate: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body already read; close error is not actionable

	if resp.StatusCode != http.StatusOK {
		// A 401 here is a revoked/dead token just as in Stream — apply
		// the same self-heal so a token first rejected during model
		// listing (e.g. at startup) is cleared and surfaced as a typed
		// AuthError. Other statuses keep the generic message.
		if resp.StatusCode == http.StatusUnauthorized {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
			if readErr != nil {
				// The body only enriches the AuthError's Code/Message;
				// parseAuthError tolerates an empty body and the 401 is
				// authoritative, so an unreadable body still self-heals.
				body = nil
			}
			return nil, authFailure(codexProviderName, c.auth, resp.StatusCode, body)
		}
		// Body drained by deferred Close; no need to read it.
		return nil, fmt.Errorf("codex: list models: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("codex: decode models: %w", err)
	}

	// The endpoint already returns the account's models in priority order.
	models := make([]ModelInfo, 0, len(result.Models))
	for _, m := range result.Models {
		if m.Visibility == "hide" { // internal models (auto-review, reserve)
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		models = append(models, ModelInfo{ID: m.Slug, Name: name})
	}
	return models, nil
}
