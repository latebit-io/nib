package session

import (
	"fmt"

	"github.com/latebit-io/nib/engine/editor"
)

// LLM, model selection, and highlighter wiring. Read-only state with
// one-time wiring at startup, plus the SwitchModel hot-swap path. State
// (llmModel, llmProfile, switchModel, highlighterFactory,
// highlighterFactoryGen) lives on Session.

// SetHighlighterFactory installs a factory used to construct highlighters
// for every editor this session creates. Pass nil to disable highlighting.
// Typical usage: the TUI wires in [highlight.NewHighlighter] at startup;
// headless binaries never call this so no grammar blobs are linked in.
//
// All currently-open editors are re-decorated to match the new factory
// (their old highlighters are closed). Concurrent factory swaps and
// editor decoration are linearized via the highlighterFactoryGen counter:
// decorateEditor re-checks the generation after installing a highlighter
// and retries if a swap happened mid-install.
func (s *Session) SetHighlighterFactory(fn editor.HighlighterFactory) {
	s.mu.Lock()
	s.highlighterFactory = fn
	s.highlighterFactoryGen++
	// Snapshot under the lock; call SetHighlighter after releasing so the
	// editor's Close() path on the old highlighter doesn't run while we
	// hold the session lock.
	open := make([]*editor.Editor, 0, len(s.editors))
	for _, e := range s.editors {
		open = append(open, e)
	}
	s.mu.Unlock()

	for _, e := range open {
		s.decorateEditor(e)
	}
}

// SetLLMInfo stores the active LLM model ID and profile name.
// Called during startup and after model switches. Guarded by mu
// for safe cross-goroutine access.
func (s *Session) SetLLMInfo(modelID, profile string) {
	s.mu.Lock()
	s.llmModel = modelID
	s.llmProfile = profile
	s.mu.Unlock()
}

// LLMModel returns the full model ID (e.g. "google/gemini-2.5-flash").
func (s *Session) LLMModel() string {
	s.mu.RLock()
	m := s.llmModel
	s.mu.RUnlock()
	return m
}

// LLMProfile returns the active LLM profile name.
func (s *Session) LLMProfile() string {
	s.mu.RLock()
	p := s.llmProfile
	s.mu.RUnlock()
	return p
}

// SetModelSwitcher injects the model-switching function. The function should
// resolve the profile, wire auth (OAuth/stored keys), create a new provider,
// and hot-swap it on the agent. Called once during startup wiring.
func (s *Session) SetModelSwitcher(fn func(profile, modelID string) (string, error)) {
	s.mu.Lock()
	s.switchModel = fn
	s.mu.Unlock()
}

// SwitchModel switches the active LLM provider and model via the injected
// switcher callback. Returns the display model name. Persistence behavior
// (if any) is determined by the callback implementation.
func (s *Session) SwitchModel(profile, modelID string) (string, error) {
	s.mu.RLock()
	switcher := s.switchModel
	s.mu.RUnlock()
	if switcher == nil {
		return "", fmt.Errorf("model switching not available")
	}
	return switcher(profile, modelID)
}
