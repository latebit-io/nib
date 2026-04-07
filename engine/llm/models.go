package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ModelInfo describes an available model from a provider.
type ModelInfo struct {
	ID   string // model identifier (e.g. "google/gemini-2.5-flash")
	Name string // human-readable name (may equal ID if provider doesn't distinguish)
}

// ModelLister can enumerate available models from a provider.
// Not all providers support this — callers should type-assert.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// listModelsResponse is the OpenAI-compatible /models response shape.
type listModelsResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
}

// ListModels queries GET {baseURL}/models for available models.
// Returns an error if the endpoint is unavailable or auth fails.
// Implements ModelLister.
func (a *AgentAPI) ListModels(ctx context.Context) ([]ModelInfo, error) {
	url := a.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // HTTP response body close rarely fails

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: list models: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result listModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode models response: %w", err)
	}

	models := make([]ModelInfo, len(result.Data))
	for i, m := range result.Data {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		models[i] = ModelInfo{ID: m.ID, Name: name}
	}
	return models, nil
}
