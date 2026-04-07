// Package wire provides shared composition logic used by both the TUI
// and headless agent binaries. It wires together LLM providers, memory
// servers, LSP, and MCP tools from environment variables and project config.
package wire

import (
	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/llmconfig"
)

// NewProvider creates an LLM provider from the merged configuration.
// Returns (nil, config, resolved) if no API key is available — the caller
// checks the provider for nil to decide whether to enable agent features.
// The Config and Resolved are always returned so callers can display model
// info and support runtime profile switching even when the provider is nil.
func NewProvider(projectRoot string) (llm.Provider, *llmconfig.Config, *llmconfig.Resolved) {
	cfg, resolved := llmconfig.Resolve(projectRoot)
	if !resolved.HasProvider() {
		return nil, cfg, resolved
	}
	return llm.NewAgentAPI(resolved.BaseURL, resolved.Model, resolved.APIKey), cfg, resolved
}
