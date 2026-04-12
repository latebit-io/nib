package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/llmconfig"
	"github.com/latebit-io/junto/engine/oauth"
	"github.com/latebit-io/junto/engine/session"
	"github.com/latebit-io/junto/engine/styleconfig"
	"github.com/latebit-io/junto/engine/wire"
	"github.com/latebit-io/junto/tui/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run wires together the engine, optional agent/LSP services, and the TUI.
func run() error { //nolint:gocognit // wiring function — inherently sequential
	// Application-level context — cancelled when run() returns (after the
	// TUI exits) so in-flight agent goroutines shut down promptly instead
	// of running until their next HTTP round-trip times out.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	// Parse args: [--debug] [file]
	args := os.Args[1:]
	debug := false
	var filePath string
	for _, a := range args {
		if a == "--debug" {
			debug = true
		} else {
			filePath = a
		}
	}

	if debug {
		logFile, err := os.OpenFile("/tmp/junto-debug.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err == nil {
			defer func() { _ = logFile.Close() }()
			slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
	} else {
		// Discard all logs — slog defaults to stderr which corrupts the alt-screen TUI.
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Determine whether the argument is a file or a directory.
	var buf *buffer.Buffer
	var projectRoot string
	if filePath != "" {
		info, err := os.Stat(filePath)
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Directory argument: use it as the project root directly.
			projectRoot, err = filepath.Abs(filePath)
			if err != nil {
				return err
			}
			buf = buffer.New()
		} else {
			// Absolutize so buffer.Path matches session.CanonPath.
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return err
			}
			buf, err = buffer.NewFromFile(absPath)
			if err != nil {
				return err
			}
			// File argument: walk up from file's parent to find project root.
			projectRoot = session.ResolveProjectRoot(filepath.Dir(absPath))
		}
	} else {
		// No argument: use cwd as project root.
		buf = buffer.New()
		projectRoot, _ = os.Getwd() // safe: Session.New normalizes via filepath.Abs
	}

	e := editor.New(buf)

	// Create session first (editor-only mode) — it serves as the agent's Workspace.
	sess := session.New(e, projectRoot)
	sess.SetContext(appCtx)

	// Discover MCP tools from .mcp.json or JUNTO_MCP env var.
	mcpResult := wire.DiscoverMCPTools(projectRoot)
	defer mcpResult.Cleanup()

	// Classify distributed memory servers and expose to the session for UI display.
	distributed := agent.DetectDistributedMemory(mcpResult.ServerNames)
	if len(distributed) > 0 {
		sess.SetDistributedMemory(distributed)
	}

	// Shared event channel — agent and LSP both write here, frontend reads one channel.
	events := make(chan event.Event, 128)

	// Start LSP servers for language intelligence.
	lspMgr := wire.InitLSP(projectRoot, events)
	if lspMgr != nil {
		sess.SetLanguageService(lspMgr)
		defer func() { _ = lspMgr.Close() }()
	}

	// Ensure demarkus binaries are installed (idempotent, skips if present).
	if err := wire.EnsureBinaries(projectRoot); err != nil {
		return fmt.Errorf("memory: install binaries: %w", err)
	}

	// Create LLM provider and agent from configuration.
	pr := wire.NewProvider(projectRoot)
	provider, llmCfg, llmResolved := pr.Provider, pr.Config, pr.Resolved
	if llmResolved != nil {
		sess.SetLLMInfo(llmResolved.Model, llmResolved.Profile)
	}

	// Resolve coding style — injected into the agent's system prompt.
	styleResult := wire.NewStyle(projectRoot)

	var ag *agent.Agent
	if provider != nil {
		// Start memory server — only needed when agent is active.
		mem, err := wire.StartMemory(projectRoot)
		if err != nil {
			return fmt.Errorf("memory: %w", err)
		}
		defer mem.Cleanup()

		opts := &agent.NewOptions{
			MemoryStore:       mem.Store,
			MemorySummary:     mem.Summary,
			DistributedMemory: distributed,
			CodingStyle:       styleResult.AgentStyle,
			Terse:             true,
		}
		if styleResult.Resolved != nil {
			opts.StyleLintCmd = styleResult.Resolved.LintCmd
			opts.StyleEvaluator = wire.NewStyleEvaluator(styleResult.Resolved, provider, llmCfg)
		}
		if lspMgr != nil {
			opts.DiagProvider = lspMgr
		}
		ag = agent.New(provider, sess, events, opts, mcpResult.Tools...)
		sess.SetAgent(ag, events)
		sess.SetMemoryStore(mem.Store)
	} else if lspMgr != nil {
		// No agent, but LSP events still need to reach the frontend.
		sess.SetEvents(events)
	}

	app := ui.NewApp(sess)
	if llmResolved != nil && llmResolved.HasProvider() {
		app.AgentPane.SetModelLabel(llmResolved.Profile + ": " + llmResolved.DisplayModel())
	}

	// Wire OAuth detection — always available, even without an initial provider.
	if pr.OAuthStore != nil {
		app.IsOAuthProfile = func(profile string) string {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return ""
			}
			return resolved.OAuthProvider
		}

		app.StoreAPIKey = func(profile, key string) error {
			return pr.KeyStore.Put(profile, key)
		}

		app.HasAPIKey = func(profile string) bool {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return false
			}
			// Has env var key or stored key.
			return resolved.HasProvider() || pr.KeyStore.HasKey(profile)
		}

		app.HasOAuthToken = func(profile string) bool {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return false
			}
			if resolved.OAuthProvider == "" {
				return false
			}
			return pr.OAuthStore.HasToken(oauth.ProviderID(resolved.OAuthProvider))
		}

		app.ConnectOAuth = func(profile string) tea.Cmd {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return func() tea.Msg {
					return ui.OAuthConnectResult(profile, fmt.Errorf("unknown profile %q", profile))
				}
			}
			switch oauth.ProviderID(resolved.OAuthProvider) {
			case oauth.ProviderOpenAI:
				return connectOpenAICmd(profile, pr.OAuthStore)
			case oauth.ProviderCopilot:
				return connectCopilotCmd(profile, pr.OAuthStore, app.Program())
			default:
				return func() tea.Msg {
					return ui.OAuthConnectResult(profile, fmt.Errorf("unknown OAuth provider: %s", resolved.OAuthProvider))
				}
			}
		}
	}

	// Profile names are always available — even without a provider,
	// the user can browse profiles and connect OAuth ones.
	app.LLMProfileNames = llmCfg.ProfileNames

	// Wire model listing and switching — closures capture ag, llmCfg, and llmResolved.
	if provider != nil && ag != nil {

		app.ListModels = func(profile string) ([]ui.ModelSelectorItem, error) {
			// Resolve the profile to get its base_url and key.
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				// Fallback: use current provider (env-only config).
				slog.Warn("llm: ListModels profile not found, falling back", "profile", profile)
				resolved = llmResolved
			}
			wire.WireOAuthProfile(resolved, pr.OAuthStore)
			wire.WireStoredKey(resolved, pr.KeyStore)

			// OAuth profiles — try API model listing, fall back to hardcoded.
			if resolved.OAuthProvider != "" {
				p := resolved.NewProvider()
				if p != nil {
					if lister, ok := p.(llm.ModelLister); ok {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						if models, err := lister.ListModels(ctx); err == nil && len(models) > 0 {
							items := make([]ui.ModelSelectorItem, len(models))
							for i, m := range models {
								items[i] = ui.ModelSelectorItem{ID: m.ID, Name: m.Name, Profile: profile}
							}
							return items, nil
						}
					}
				}
				// Fallback to hardcoded list.
				return oauthModels(resolved.OAuthProvider, profile, resolved.Model), nil
			}

			p := resolved.NewProvider()
			if p == nil {
				return nil, fmt.Errorf("no API key for profile %q", profile)
			}
			lister, ok := p.(llm.ModelLister)
			if !ok {
				return nil, fmt.Errorf("provider does not support model listing")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			models, err := lister.ListModels(ctx)
			if err != nil {
				return nil, err
			}
			items := make([]ui.ModelSelectorItem, len(models))
			for i, m := range models {
				items[i] = ui.ModelSelectorItem{ID: m.ID, Name: m.Name, Profile: profile}
			}
			return items, nil
		}

		app.SwitchModel = func(profile, modelID string) (string, error) {
			// Resolve the target profile.
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				slog.Warn("llm: profile not found, falling back", "profile", profile)
				resolved = llmResolved
			}
			slog.Debug("llm: switch resolving", "profile", profile, "modelID", modelID, "resolvedModel", resolved.Model)
			// Empty modelID means use the profile's default model.
			if modelID != "" {
				resolved.Model = modelID
			}
			wire.WireOAuthProfile(resolved, pr.OAuthStore)
			wire.WireStoredKey(resolved, pr.KeyStore)
			newProvider := resolved.NewProvider()
			if newProvider == nil {
				return "", fmt.Errorf("no API key available for profile %q", profile)
			}
			ag.SetProvider(newProvider)
			provider = newProvider
			llmResolved = resolved
			sess.SetLLMInfo(resolved.Model, resolved.Profile)
			slog.Info("llm: switched model", "profile", profile, "model", modelID)
			return resolved.DisplayModel(), nil
		}
	}

	// Wire coding style cycling — closures capture ag, styleResult, and the style config.
	if styleResult.Resolved != nil {
		app.SetStyleName(styleResult.Resolved.Name)
	}
	// Shared state for style cycling and evaluator toggle — both closures
	// need to know the current resolved style to stay in sync.
	currentResolved := styleResult.Resolved
	evaluatorActive := false

	if ag != nil {
		styleNames := styleResult.Config.StyleNames()
		if len(styleNames) > 0 {
			currentStyleKey := "" // track the current style key for cycling
			if styleResult.Config.Active != "" {
				currentStyleKey = styleResult.Config.Active
			}
			app.CycleStyle = func() string {
				// Find current index, advance to next (with "none" after the last).
				idx := -1
				for i, name := range styleNames {
					if name == currentStyleKey {
						idx = i
						break
					}
				}
				idx++
				if idx >= len(styleNames) {
					// Wrap to "none" — disable style enforcement.
					currentStyleKey = ""
					currentResolved = nil
					ag.SetStyle(nil, nil)
					if evaluatorActive {
						ag.SetEvaluator(nil)
						evaluatorActive = false
						app.SetEvaluatorEnabled(false)
					}
					slog.Info("style: disabled")
					return ""
				}
				currentStyleKey = styleNames[idx]
				s := styleResult.Config.Styles[currentStyleKey]
				data := agent.NewCodingStyleData(s.Name, wire.ConvertRules(s.Rules))
				lintCmd := s.LintCmd
				if len(lintCmd) == 0 {
					lintCmd = styleResult.DefaultLintCmd
				}
				ag.SetStyle(data, lintCmd)

				// Update the resolved style for the evaluator.
				currentResolved = &styleconfig.Resolved{
					Name:           s.Name,
					Rules:          s.Rules,
					LintCmd:        lintCmd,
					Evaluator:      s.Evaluator,
					EvaluatorModel: s.EvaluatorModel,
				}

				// Rebuild evaluator if it's active, using the new style's rules.
				if evaluatorActive {
					eval := wire.ForceStyleEvaluator(currentResolved, provider, llmCfg)
					ag.SetEvaluator(eval) // nil is fine — disables if no provider
					if eval == nil {
						evaluatorActive = false
						app.SetEvaluatorEnabled(false)
					}
				}

				slog.Info("style: switched", "style", s.Name)
				return s.Name
			}
		}
	}

	// Wire evaluator toggle — Alt+V enables/disables the style evaluator at runtime.
	if ag != nil {
		// If the config has evaluator enabled at startup, set the initial state.
		if currentResolved != nil && currentResolved.Evaluator {
			eval := wire.NewStyleEvaluator(currentResolved, provider, llmCfg)
			if eval != nil {
				ag.SetEvaluator(eval)
				evaluatorActive = true
				app.SetEvaluatorEnabled(true)
			}
		}
		app.ToggleEvaluator = func(enabled bool) bool {
			if currentResolved == nil {
				return false // no style active
			}
			if enabled {
				eval := wire.ForceStyleEvaluator(currentResolved, provider, llmCfg)
				if eval == nil {
					return false // no provider available
				}
				ag.SetEvaluator(eval)
				evaluatorActive = true
				slog.Info("evaluator: enabled")
				return true
			}
			ag.SetEvaluator(nil)
			evaluatorActive = false
			slog.Info("evaluator: disabled")
			return false
		}
	}

	// Wire terse toggle — Alt+T enables/disables terse output mode at runtime.
	// Terse is enabled by default to reduce output token costs.
	if ag != nil {
		app.SetTerse(true)
		app.ToggleTerse = func(enabled bool) bool {
			ag.SetTerse(enabled)
			if enabled {
				slog.Info("terse mode: enabled")
			} else {
				slog.Info("terse mode: disabled")
			}
			return enabled
		}
	}

	// Agent typing speed (words per minute)
	if wpmStr := os.Getenv("JUNTO_TYPING_WPM"); wpmStr != "" {
		if wpm, err := strconv.Atoi(wpmStr); err == nil && wpm > 0 {
			app.Editor.TypingWPM = wpm
		}
	}
	// Instant-apply mode: skip typing animation, apply edits atomically.
	if os.Getenv("JUNTO_INSTANT_APPLY") == "1" {
		app.Editor.InstantApply = true
	}
	p := tea.NewProgram(&app,
		tea.WithoutSignalHandler(), // let Ctrl+C reach us as a key event
	)
	app.SetProgram(p)

	shutdown := func() {
		app.CloseWatcher()
		appCancel() // signal agent goroutines before teardown
		sess.Close()
	}

	_, err := p.Run()
	shutdown()
	return err
}

// connectOpenAICmd returns a tea.Cmd that runs the OpenAI browser OAuth flow.
func connectOpenAICmd(profile string, store *oauth.Store) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		err := oauth.OpenAIBrowserFlow(ctx, store, nil)
		return ui.OAuthConnectResult(profile, err)
	}
}

// connectCopilotCmd returns a tea.Cmd that runs the GitHub Copilot device code
// flow. The device code request, instruction delivery, and polling all run
// off the TUI thread.
func connectCopilotCmd(profile string, store *oauth.Store, p *tea.Program) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		dc, err := oauth.RequestCopilotDeviceCode(ctx)
		cancel()
		if err != nil {
			return ui.OAuthConnectResult(profile, fmt.Errorf("request device code: %w", err))
		}

		// Deliver the instruction to the TUI via Program.Send so the user
		// sees the code immediately while we poll in the background.
		instruction := fmt.Sprintf("Visit %s and enter code: %s", dc.VerificationURI, dc.UserCode)
		p.Send(ui.OAuthInstruction(profile, instruction))

		pollCtx, pollCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer pollCancel()
		err = oauth.CompleteCopilotDeviceFlow(pollCtx, store, dc)
		return ui.OAuthConnectResult(profile, err)
	}
}

// codexModels are available via the ChatGPT Codex subscription endpoint.
var codexModels = []string{
	"gpt-5.1-codex",
	"gpt-5.1-codex-max",
	"gpt-5.1-codex-mini",
	"gpt-5.2",
	"gpt-5.2-codex",
	"gpt-5.3-codex",
	"gpt-5.4",
	"gpt-5.4-mini",
}

// oauthModels returns the known model list for an OAuth provider.
// OAuth endpoints don't support /models, so we maintain the list statically.
func oauthModels(provider, profile, defaultModel string) []ui.ModelSelectorItem {
	var models []string
	switch provider {
	case "openai":
		models = codexModels
	default:
		models = []string{defaultModel}
	}

	items := make([]ui.ModelSelectorItem, 0, len(models))
	for _, m := range models {
		if m == defaultModel {
			items = append([]ui.ModelSelectorItem{{ID: m, Name: m, Profile: profile}}, items...)
		} else {
			items = append(items, ui.ModelSelectorItem{ID: m, Name: m, Profile: profile})
		}
	}
	if len(items) == 0 {
		items = append(items, ui.ModelSelectorItem{ID: defaultModel, Name: defaultModel, Profile: profile})
	}
	return items
}
