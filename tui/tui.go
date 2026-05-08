// Package tui provides the recommended entry point for running nib's
// reference terminal UI. Callers construct a [Config], call [New] to
// wire the application, then [App.Run] to start the Bubble Tea event
// loop. The tui/ui sub-package remains importable for advanced use
// cases (custom panes, embedding) but is not the stable surface.
package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/syntax"
	"github.com/latebit-io/nib/tui/ui"
)

// ModelSelectorItem re-exports the UI's model selector item so facade
// callers don't need to import tui/ui directly.
type ModelSelectorItem = ui.ModelSelectorItem

// OAuthConnectResult creates a message signaling OAuth flow completion.
func OAuthConnectResult(profile string, err error) tea.Msg {
	return ui.OAuthConnectResult(profile, err)
}

// OAuthInstruction creates a message delivering an intermediate OAuth
// instruction (e.g., device code) to the UI.
func OAuthInstruction(profile, instruction string) tea.Msg {
	return ui.OAuthInstruction(profile, instruction)
}

// Config holds everything the TUI needs to run.
type Config struct {
	// Session is the engine session (required).
	Session *session.Session

	// Events is the shared event channel the TUI reads from.
	Events chan event.Event

	// Agent is the coding agent (optional — nil for editor-only mode).
	Agent *agent.Agent

	// AgentCallbacks holds callbacks that require a live agent.
	// Ignored when Agent is nil.
	AgentCallbacks AgentCallbacks

	// LLM holds LLM-related callbacks (model listing, profiles).
	// Optional — nil disables model switching UI.
	LLM *LLMCallbacks

	// OAuth holds OAuth connection callbacks.
	// Optional — nil disables OAuth UI flows.
	OAuth *OAuthCallbacks

	// HighlighterFactory builds a syntax highlighter for a given file
	// path. The TUI installs the returned highlighter on each editor it
	// constructs in its per-file pool. Pass nil to disable highlighting
	// (e.g. tests).
	HighlighterFactory syntax.HighlighterFactory
}

// AgentCallbacks groups callbacks that require a live agent.
type AgentCallbacks struct {
	// OnDialChange is called when the autonomy dial changes.
	OnDialChange func(level session.AutonomyLevel)

	// CycleStyle advances to the next coding style. Returns display name or "".
	CycleStyle func() string

	// ToggleEvaluator enables/disables the style evaluator. Returns new state.
	ToggleEvaluator func(enabled bool) bool

	// ToggleTerse enables/disables terse mode. Returns new state.
	ToggleTerse func(enabled bool) bool

	// InitialStyleName is the coding style name at startup.
	InitialStyleName string

	// InitialEvaluatorEnabled is the evaluator state at startup.
	InitialEvaluatorEnabled bool

	// InitialTerse is the terse mode state at startup.
	InitialTerse bool
}

// LLMCallbacks groups LLM-related UI callbacks.
type LLMCallbacks struct {
	// ListModels returns available models for a profile.
	ListModels func(profile string) ([]ModelSelectorItem, error)

	// ProfileNames returns available LLM profile names.
	ProfileNames func() []string

	// ModelLabel is the initial model label for the agent pane header.
	ModelLabel string

	// StoreAPIKey saves an API key for a profile.
	StoreAPIKey func(profile, key string) error

	// HasAPIKey reports whether credentials exist for a profile.
	HasAPIKey func(profile string) bool

	// IsOAuthProfile reports whether a profile uses OAuth (returns provider ID or "").
	IsOAuthProfile func(profile string) string
}

// OAuthCallbacks groups OAuth-specific UI callbacks.
type OAuthCallbacks struct {
	// ConnectOAuth starts an OAuth flow for the given profile.
	ConnectOAuth func(profile string) tea.Cmd

	// HasOAuthToken reports whether a valid token exists for a profile.
	HasOAuthToken func(profile string) bool
}

// App is a fully-wired TUI application ready to run.
type App struct {
	model   *ui.AppModel
	program *tea.Program
	agent   *agent.Agent
	events  chan event.Event
	session *session.Session
}

// New constructs the TUI from the given config. The returned App is
// ready to run but has not started Bubble Tea yet — callers can set
// additional callbacks on [App.Model] before calling [App.Run].
func New(cfg Config) *App {
	app := ui.NewApp(cfg.Session)
	appPtr := &app

	if cfg.HighlighterFactory != nil {
		appPtr.SetHighlighterFactory(cfg.HighlighterFactory)
	}

	// Wire LLM callbacks.
	if cfg.LLM != nil {
		appPtr.ListModels = cfg.LLM.ListModels
		appPtr.LLMProfileNames = cfg.LLM.ProfileNames
		appPtr.StoreAPIKey = cfg.LLM.StoreAPIKey
		appPtr.HasAPIKey = cfg.LLM.HasAPIKey
		appPtr.IsOAuthProfile = cfg.LLM.IsOAuthProfile
		if cfg.LLM.ModelLabel != "" {
			appPtr.AgentPane.SetModelLabel(cfg.LLM.ModelLabel)
		}
	}

	// Wire OAuth callbacks.
	if cfg.OAuth != nil {
		appPtr.ConnectOAuth = cfg.OAuth.ConnectOAuth
		appPtr.HasOAuthToken = cfg.OAuth.HasOAuthToken
	}

	// Wire agent callbacks.
	if cfg.Agent != nil {
		appPtr.AgentPane.SetHasAgent(true)
		appPtr.OnDialChange = cfg.AgentCallbacks.OnDialChange
		appPtr.CycleStyle = cfg.AgentCallbacks.CycleStyle
		appPtr.ToggleEvaluator = cfg.AgentCallbacks.ToggleEvaluator
		appPtr.ToggleTerse = cfg.AgentCallbacks.ToggleTerse
		if cfg.AgentCallbacks.InitialStyleName != "" {
			appPtr.SetStyleName(cfg.AgentCallbacks.InitialStyleName)
		}
		if cfg.AgentCallbacks.InitialEvaluatorEnabled {
			appPtr.SetEvaluatorEnabled(true)
		}
		if cfg.AgentCallbacks.InitialTerse {
			appPtr.SetTerse(true)
		}
	}

	p := tea.NewProgram(appPtr, tea.WithoutSignalHandler())
	appPtr.SetProgram(p)

	return &App{
		model:   appPtr,
		program: p,
		agent:   cfg.Agent,
		events:  cfg.Events,
		session: cfg.Session,
	}
}

// Model returns the underlying AppModel for advanced wiring (e.g.,
// model switcher callbacks set after construction).
func (a *App) Model() *ui.AppModel { return a.model }

// Program returns the tea.Program for sending async messages.
func (a *App) Program() *tea.Program { return a.program }

// Run starts the Bubble Tea event loop and blocks until the user
// quits. On return, all resources (watcher, agent) are cleaned up.
// The caller is responsible for closing the session and any deferred
// resources (LSP, memory, MCP) after Run returns.
func (a *App) Run() error {
	_, err := a.program.Run()
	a.shutdown()
	return err
}

// shutdown tears down resources in the correct order after the TUI exits.
func (a *App) shutdown() {
	a.model.CloseWatcher()
	if a.agent != nil {
		drainDone := make(chan struct{})
		go func() {
			for {
				select {
				case <-drainDone:
					return
				case <-a.events:
				}
			}
		}()
		a.agent.Close()
		close(drainDone)
	}
}
