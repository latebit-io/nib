// Package wire provides shared composition logic used by both the TUI
// and headless agent binaries. It wires together LLM providers, memory
// servers, LSP, and MCP tools from environment variables and project config.
package wire

import (
	"os"

	"github.com/latebit-io/junto/engine/llm"
)

// defaultBaseURL is the LLM provider base URL when LLM_BASE_URL is unset.
const defaultBaseURL = "https://openrouter.ai/api/v1"

// defaultModel is the LLM model when LLM_MODEL is unset.
const defaultModel = "google/gemini-2.5-flash"

// NewProvider creates an LLM provider from environment variables.
// Returns nil if LLM_API_KEY is not set. The caller should check for nil
// to decide whether to enable agent features.
func NewProvider() llm.Provider {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		return nil
	}

	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	model := os.Getenv("LLM_MODEL")
	if model == "" {
		model = defaultModel
	}

	return llm.NewAgentAPI(baseURL, model, apiKey)
}
