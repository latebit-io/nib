package session

import (
	"fmt"
)

// LLM and model selection wiring. Read-only state with one-time wiring at
// startup, plus the SwitchModel hot-swap path. State (llmModel,
// llmProfile, switchModel) lives on Session.
//
// Highlighter factory wiring no longer lives on Session: the frontend
// owns its own editor controllers and decorates them on construction.

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
