// Package tui provides the recommended entry point for running nib's
// reference terminal UI. Callers construct a [Config], call [New] to
// wire the application, then [App.Run] to start the Bubble Tea event
// loop. The tui/ui sub-package remains importable for advanced use
// cases (custom panes, embedding) but is not the stable surface.
package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/syntax"
	"github.com/latebit-io/nib/kit"
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

// AgentResolver returns the live agent lifecycle the TUI shuts down on
// exit, or a nil interface when no agent exists yet. It is a resolver
// rather than a value because the agent may be constructed lazily after
// the TUI starts. Implementations MUST return a true nil interface when
// there is no agent — never a typed-nil pointer, which would satisfy a
// `!= nil` check and panic when Close is dispatched on the nil receiver.
type AgentResolver func() kit.AgentLifecycle

// Config holds everything the TUI needs to run.
type Config struct {
	// Session is the engine session (required).
	Session *session.Session

	// Events is the shared event channel the TUI reads from.
	Events chan event.Event

	// Agent resolves the live agent the TUI shuts down on exit.
	// Optional — nil (or a resolver returning nil) for editor-only
	// mode. *coding.Agent satisfies [kit.AgentLifecycle] via its
	// embedded kit-agent handle; future agent shapes (research,
	// refactor) plug in by satisfying the same port.
	//
	// A resolver rather than a value because the agent may be built
	// lazily after the TUI starts (no LLM credentials at startup →
	// the model switcher constructs it on first connect). A value
	// snapshot would capture nil forever — leaking the lazily-built
	// agent's goroutines and, when the snapshot is a typed-nil
	// pointer, panicking on the shutdown Close. The resolver MUST
	// return a true nil interface (not a typed-nil pointer) when no
	// agent exists.
	Agent AgentResolver

	// AgentCallbacks holds generic frontend callbacks that any
	// kit-level agent can supply. Wired only when Agent is non-nil.
	AgentCallbacks AgentCallbacks

	// CodingCallbacks holds coding-flavored callbacks that depend on
	// coding-agent concepts. Wired only when Agent is non-nil.
	CodingCallbacks CodingCallbacks

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

// AgentCallbacks groups generic frontend callbacks that any kit-level
// agent can supply. A non-coding kit consumer (research agent, refactor
// agent) populates these without touching CodingCallbacks. Today the
// surface is small (output verbosity); promote a CodingCallbacks field
// up to AgentCallbacks when its parameter types and semantics no
// longer reference coding-only state.
type AgentCallbacks struct {
	// ToggleTerse enables/disables terse output mode. Returns the
	// new state.
	ToggleTerse func(enabled bool) bool

	// InitialTerse is the terse mode state at startup.
	InitialTerse bool
}

// CodingCallbacks groups frontend callbacks that depend on coding-agent
// concepts. Currently empty — kept as a wiring seam for future coding-
// flavored callbacks (the per-PR diff stays small when one returns).
type CodingCallbacks struct{}

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
	agent   AgentResolver
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

	// Wire agent callbacks. Generic and coding-flavored bundles are
	// populated independently — a non-coding consumer wires
	// AgentCallbacks alone and leaves CodingCallbacks zero.
	if cfg.Agent != nil {
		// Reflect whether an agent actually exists at construction.
		// It may be built lazily after startup (no LLM credentials
		// yet), in which case the composition root flips has-agent on
		// when it builds the agent.
		if cfg.Agent() != nil {
			appPtr.AgentPane.SetHasAgent(true)
		}

		// Generic.
		appPtr.ToggleTerse = cfg.AgentCallbacks.ToggleTerse
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

// FlushDirtyBuffers asks the TUI to save all dirty buffers to disk,
// blocking until the save completes or ctx fires. Returns the canonical
// paths of files saved and the first error (nil on full success).
//
// Routes through [tea.Program.Send] so the actual I/O runs on the
// Bubble Tea Update goroutine — the same goroutine that mutates
// [engine/buffer.Buffer] state via keystroke handlers.
// [engine/buffer.Buffer] holds no internal lock; the goroutine-affinity
// rule is how concurrent keystroke + agent-edit access stays race-free.
// Calling [session.Session.SaveDirtyBuffers] directly from a different
// goroutine (e.g. the agent's tool-dispatch goroutine) would race the
// Update goroutine's buffer mutations.
//
// Intended as the implementation of
// [coding/agent.NewOptions.FlushDirtyBuffers] in the flagship binary.
func (a *App) FlushDirtyBuffers(ctx context.Context) ([]string, error) {
	// Buffered so the Update-goroutine handler can write the result
	// without blocking even if ctx fires here first and we stop
	// listening.
	result := make(chan ui.FlushResult, 1)
	a.program.Send(ui.FlushDirtyBuffersMsg{Result: result})
	select {
	case res := <-result:
		return res.Saved, res.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Program returns the tea.Program for sending async messages.
func (a *App) Program() *tea.Program { return a.program }

// SetAgentCallbacks installs generic agent callbacks. Late-binding:
// callable any time — pre-Run (during startup wiring) or post-Run
// (the lazy-OAuth model-switcher path). Idempotent — last call wins.
//
// The callbacks are routed through [tea.Program.Send] into the
// AppModel's Update goroutine so the field assignments happen on
// Bubble Tea's single-threaded event loop (race-free regardless of
// caller goroutine). Bubble Tea's Send is *blocking* on its
// unbuffered channel pre-Run, so the dispatch runs in a spawned
// goroutine — the caller never blocks; the goroutine completes
// when Run begins pumping and the message lands. Field assignment
// is therefore asynchronous: the caller cannot rely on the field
// being set when SetAgentCallbacks returns. In practice this is
// fine because the assigned fields drive keystroke handlers, and
// no keystroke can fire until Run has started and processed the
// queued messages.
func (a *App) SetAgentCallbacks(cb AgentCallbacks) {
	msg := ui.SetAgentCallbacksMsg(cb.ToggleTerse, cb.InitialTerse)
	go a.program.Send(msg)
}

// SetCodingCallbacks installs coding-flavored agent callbacks.
// Same any-time semantics, race-avoidance rationale, and async
// caveat as [App.SetAgentCallbacks]. Currently a no-op — the
// callback set is empty after the autonomy-dial removal but the
// wiring seam is kept so future coding-flavored callbacks land
// with a tiny diff.
func (a *App) SetCodingCallbacks(_ CodingCallbacks) {
	go a.program.Send(ui.SetCodingCallbacksMsg())
}

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
	if a.agent == nil {
		return
	}
	// Resolve the live agent. nil means no agent was ever built
	// (editor-only mode, or startup without LLM credentials) — nothing
	// to close.
	ag := a.agent()
	if ag == nil {
		return
	}
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
	ag.Close()
	close(drainDone)
}
