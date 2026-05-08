package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/ai/oauth"
	"github.com/latebit-io/nib/coding/agent"
	codingcmd "github.com/latebit-io/nib/coding/command"
	"github.com/latebit-io/nib/coding/event"
	codingmemory "github.com/latebit-io/nib/coding/memory"
	"github.com/latebit-io/nib/coding/prompts"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/coding/wire"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/capture/demarkus"
	"github.com/latebit-io/nib/engine/editor"
	"github.com/latebit-io/nib/engine/highlight"
	"github.com/latebit-io/nib/engine/runconfig"
	"github.com/latebit-io/nib/engine/styleconfig"
	"github.com/latebit-io/nib/engine/validate"
	"github.com/latebit-io/nib/engine/validate/architecture"
	"github.com/latebit-io/nib/engine/validate/goparse"
	"github.com/latebit-io/nib/engine/validate/lintstage"
	"github.com/latebit-io/nib/engine/validate/treesitter"
	kitcmd "github.com/latebit-io/nib/kit/command"
	cmdloader "github.com/latebit-io/nib/kit/command/loader"
	nibTui "github.com/latebit-io/nib/tui"
	tuicmd "github.com/latebit-io/nib/tui/command"
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
		logPath, err := brand.DebugLogPath("debug.log")
		if err != nil {
			fmt.Fprintf(os.Stderr, "debug log path: %v — proceeding without debug log\n", err)
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		} else if logFile, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); openErr != nil {
			fmt.Fprintf(os.Stderr, "open debug log %s: %v — proceeding without debug log\n", logPath, openErr)
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		} else {
			defer func() {
				if err := logFile.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "warning: close debug log: %v\n", err)
				}
			}()
			slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
	} else {
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
			projectRoot, err = filepath.Abs(filePath)
			if err != nil {
				return err
			}
			buf = buffer.New()
		} else {
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return err
			}
			buf, err = buffer.NewFromFile(absPath)
			if err != nil {
				return err
			}
			projectRoot = session.ResolveProjectRoot(filepath.Dir(absPath))
		}
	} else {
		buf = buffer.New()
		var err error
		projectRoot, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("determine project root: %w", err)
		}
	}

	e := editor.New(buf)
	if buf.Path != "" {
		e.SetHighlighter(highlight.NewHighlighter(buf.Path))
	}

	sess := session.New(e, projectRoot)
	sess.SetContext(appCtx)
	sess.SetHighlighterFactory(highlight.NewHighlighter)

	// Discover MCP tools from .mcp.json or the brand-prefixed MCP env var.
	mcpResult := wire.DiscoverMCPTools(projectRoot)
	defer mcpResult.Cleanup()

	distributed := codingmemory.DetectDistributedMemory(mcpResult.ServerNames)
	if len(distributed) > 0 {
		sess.SetDistributedMemory(distributed)
	}

	events := make(chan event.Event, 128)

	lspMgr := wire.InitLSP(projectRoot, events)
	if lspMgr != nil {
		sess.SetLanguageService(lspMgr)
		defer func() { _ = lspMgr.Close() }()
	}

	if err := wire.EnsureBinaries(appCtx, projectRoot); err != nil {
		return fmt.Errorf("memory: install binaries: %w", err)
	}

	pr := wire.NewProvider(projectRoot)
	provider, llmCfg, llmResolved := pr.Provider, pr.Config, pr.Resolved
	if llmResolved != nil {
		sess.SetLLMInfo(llmResolved.Model, llmResolved.Profile)
	}

	styleResult := wire.NewStyle(projectRoot)

	smokeCfg := runconfig.Load(projectRoot)
	if smokeCfg.Skipped {
		slog.Info("smoke: skipped", "reason", smokeCfg.SkipReason)
	} else {
		slog.Info("smoke: configured", "command", smokeCfg.Command, "source", smokeCfg.Source)
	}

	slog.Debug("startup: provider resolved", "hasProvider", provider != nil)

	mem, err := wire.StartMemory(appCtx, projectRoot)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	defer mem.Cleanup()
	sess.SetMemoryStore(mem.Store)

	if os.Getenv(brand.EnvKeyCaptureDisabled) == "" {
		captureSink := demarkus.New(mem.Store, sess.SessionID(), demarkus.Config{})
		sess.SetEventSink(captureSink)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := captureSink.Close(ctx); err != nil {
				slog.Warn("capture sink close failed", "err", err)
			}
		}()
	}

	sess.SetEvents(events)

	var ag *agent.Agent
	var switchMu sync.Mutex

	buildAgent := func(p llm.Provider) *agent.Agent {
		opts := &agent.NewOptions{
			MemoryStore:       mem.Store,
			MemorySummary:     mem.Summary,
			DistributedMemory: distributed,
			CodingStyle:       styleResult.AgentStyle,
			Terse:             true,
			SmokeConfig:       smokeCfg,
		}
		if styleResult.Resolved != nil {
			opts.Linters = styleResult.Linters
			opts.StyleEvaluator = wire.NewStyleEvaluator(styleResult.Resolved, p, llmCfg)
		}
		if lspMgr != nil {
			opts.DiagProvider = lspMgr
		}
		if os.Getenv(brand.EnvKeyValidatorsDisabled) == "" {
			opts.ValidationPipeline = validate.NewPipeline(
				goparse.Validator{},
				treesitter.New(highlight.LanguageFor),
				architecture.New(styleResult.Architecture, highlight.LanguageFor),
				lintstage.New(styleResult.PerFileLinters.Linters),
			)
		}
		return agent.New(p, sess, events, opts, mcpResult.Tools...)
	}

	// Build the agent now if credentials were already available at startup.
	if provider != nil {
		ag = buildAgent(provider)
		sess.SetAgent(ag, events)
	}

	// Model registry — fetches from models.dev, caches locally, refreshes hourly.
	var registryCacheDir string
	if cfgPath := llmconfig.GlobalConfigPath(); cfgPath != "" {
		registryCacheDir = filepath.Dir(cfgPath)
	} else if cacheDir, err := os.UserCacheDir(); err == nil {
		registryCacheDir = filepath.Join(cacheDir, brand.ConfigDirName)
	} else {
		slog.Warn("model registry: cannot resolve cache directory, using temp")
		registryCacheDir = filepath.Join(os.TempDir(), brand.ConfigDirName)
	}
	modelRegistry := llm.NewModelRegistry(registryCacheDir, time.Hour)

	// Build LLM callbacks — available unconditionally for model browsing.
	var modelLabel string
	if llmResolved != nil && llmResolved.HasProvider() {
		modelLabel = llmResolved.Profile + ": " + llmResolved.DisplayModel()
	}
	llmCallbacks := &nibTui.LLMCallbacks{
		ProfileNames: llmCfg.ProfileNames,
		ModelLabel:   modelLabel,
		IsOAuthProfile: func(profile string) string {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return ""
			}
			return resolved.OAuthProvider
		},
		StoreAPIKey: func(profile, key string) error {
			if pr.KeyStore == nil {
				return fmt.Errorf("key storage not available")
			}
			return pr.KeyStore.Put(profile, key)
		},
		HasAPIKey: func(profile string) bool {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return false
			}
			return resolved.HasProvider() || (pr.KeyStore != nil && pr.KeyStore.HasKey(profile))
		},
		ListModels: func(profile string) ([]nibTui.ModelSelectorItem, error) {
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				return nil, fmt.Errorf("unknown profile %q", profile)
			}
			llmconfig.WireOAuth(resolved, pr.OAuthStore)
			llmconfig.WireStoredKey(resolved, pr.KeyStore)

			if !resolved.HasProvider() {
				return nil, fmt.Errorf("no credentials for profile %q", profile)
			}

			ctx, cancel := context.WithTimeout(appCtx, 10*time.Second)
			defer cancel()

			if regID := registryProvider(profile); regID != "" {
				filter := registryFilter(profile)
				models, err := modelRegistry.Models(ctx, regID, filter)
				if err != nil {
					slog.Debug("llm: registry lookup failed, trying provider API", "profile", profile, "err", err)
				} else {
					return modelsToItems(models, profile, resolved.Model), nil
				}
			}

			p := resolved.NewProvider()
			if p == nil {
				return nil, fmt.Errorf("no API key for profile %q", profile)
			}
			lister, ok := p.(llm.ModelLister)
			if !ok {
				return nil, fmt.Errorf("provider does not support model listing")
			}
			models, err := lister.ListModels(ctx)
			if err != nil {
				return nil, err
			}
			if profile == "chatgpt" {
				filtered := models[:0]
				for _, m := range models {
					if strings.Contains(m.ID, "codex") || strings.HasPrefix(m.ID, "gpt-5") {
						filtered = append(filtered, m)
					}
				}
				models = filtered
			}
			return modelsToItems(models, profile, resolved.Model), nil
		},
	}

	// Construct the TUI via the facade.
	tuiApp := nibTui.New(nibTui.Config{
		Session: sess,
		Events:  events,
		Agent:   ag,
		LLM:     llmCallbacks,
	})
	app := tuiApp.Model()

	// Compose the slash-command registry. Domain commands (/compact)
	// live in coding/command; frontend-shaped commands (/clear, /quit)
	// live in tui/command; the kit-shipped /help is registered last
	// so it sees every other command. /quit asks the program to
	// terminate via QuitMsg — same path Ctrl+C takes — so cleanup in
	// App.Run's deferred shutdown still fires. /clear and /compact
	// are registered eagerly even when ag is nil (no LLM credentials):
	// they capture *agent.Agent through closures resolved at dispatch
	// time so the model-switcher's lazy agent construction wires up
	// without a re-registration step.
	cmdRegistry := kitcmd.NewRegistry()
	compactor := agentCompactor{getAgent: func() *agent.Agent { return ag }}
	if err := cmdRegistry.Register(codingcmd.NewCompact(
		compactor, agent.ErrNothingToCompact, agent.ErrNoConversation,
	)); err != nil {
		return fmt.Errorf("register /compact: %w", err)
	}
	resetter := agentResetter{getAgent: func() *agent.Agent { return ag }}
	if err := cmdRegistry.Register(tuicmd.NewClear(resetter, app.AgentPane)); err != nil {
		return fmt.Errorf("register /clear: %w", err)
	}
	// Markdown-defined PromptCommands. Project-local commands live
	// under <projectRoot>/.project/commands/ and shadow global
	// commands at <UserConfigDir>/nib/commands/ via the registry's
	// precedence model. Per-file parse errors are logged but never
	// abort startup — the user's other commands still load. A
	// missing directory is a no-op (most projects won't have one).
	loadCommandDir := func(dir string, kind kitcmd.SourceKind) {
		cmds, err := cmdloader.LoadDir(dir, kind)
		if err != nil {
			slog.Warn("commands: partial load", "dir", dir, "err", err)
		}
		for _, c := range cmds {
			if regErr := cmdRegistry.Register(c); regErr != nil {
				slog.Warn("commands: register failed",
					"name", c.Definition().Name,
					"path", c.Definition().Source.Path,
					"err", regErr)
			}
		}
	}
	loadCommandDir(filepath.Join(projectRoot, ".project", "commands"), kitcmd.SourceProject)
	if userCfgDir, err := os.UserConfigDir(); err == nil {
		loadCommandDir(filepath.Join(userCfgDir, brand.ConfigDirName, "commands"), kitcmd.SourceGlobal)
	} else {
		slog.Debug("commands: skipping global dir, UserConfigDir unavailable", "err", err)
	}

	if err := cmdRegistry.Register(tuicmd.NewQuit(func() {
		// Send must run off the Update goroutine. Program.msgs is an
		// unbuffered channel; Send blocks until the loop reads, but
		// the loop is parked inside this very Handle call. Synchronous
		// Send self-deadlocks. Spawning a goroutine deposits the
		// QuitMsg on a side stack, lets Handle return, and the loop
		// drains the next tick. Bubble Tea's idiomatic alternative —
		// returning tea.Quit as a Cmd from Update — would require
		// threading tea.Cmd through kit.Session, which is rejected
		// per the v3 plan: kit Session stays {Display, SubmitPrompt}.
		go tuiApp.Program().Quit()
	})); err != nil {
		return fmt.Errorf("register /quit: %w", err)
	}
	if err := cmdRegistry.Register(kitcmd.NewHelp(cmdRegistry)); err != nil {
		return fmt.Errorf("register /help: %w", err)
	}
	// Busy probe: refuse dispatch only when a turn is actively in
	// flight (running AND not parked at AwaitInput). The agent is
	// "running" between the first prompt and the final AgentDone —
	// most of that span it sits parked waiting for the user, which
	// IS the right moment for /clear and /compact. Restricting on
	// IsRunning alone would gate every command behind a fresh
	// session restart. nil ag (no LLM credentials) leaves the probe
	// nil so /help and /quit work even before the agent is built.
	var cmdBusy func() bool
	if ag != nil {
		cmdBusy = func() bool { return ag.IsRunning() && !ag.IsWaiting() }
	}
	app.AgentPane.SetCommandDispatch(cmdRegistry, cmdBusy, appCtx)

	// OAuth callbacks — wired post-construction because ConnectOAuth
	// needs tuiApp.Program() which only exists after New().
	if pr.OAuthStore != nil {
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
					return nibTui.OAuthConnectResult(profile, fmt.Errorf("unknown profile %q", profile))
				}
			}
			switch oauth.ProviderID(resolved.OAuthProvider) {
			case oauth.ProviderOpenAI:
				return connectOpenAICmd(profile, pr.OAuthStore)
			case oauth.ProviderCopilot:
				return connectCopilotCmd(profile, pr.OAuthStore, tuiApp.Program())
			default:
				return func() tea.Msg {
					return nibTui.OAuthConnectResult(profile, fmt.Errorf("unknown OAuth provider: %s", resolved.OAuthProvider))
				}
			}
		}
	}

	// Shared state for style cycling and evaluator toggle.
	currentResolved := styleResult.Resolved
	evaluatorActive := false

	// wireAgentHandlers installs the UI callbacks that require a live agent.
	// Called once — on startup (when credentials exist) or on the first
	// successful connect via the model switcher.
	wireAgentHandlers := func() {
		ag.SetAutonomous(true)

		app.OnDialChange = func(level session.AutonomyLevel) {
			ag.SetAutonomous(level.AutoApproveEdits())
		}

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

		if styleResult.Resolved != nil {
			app.SetStyleName(styleResult.Resolved.Name)
		}
		styleNames := styleResult.Config.StyleNames()
		if len(styleNames) > 0 {
			currentStyleKey := ""
			if styleResult.Config.Active != "" {
				currentStyleKey = styleResult.Config.Active
			}
			app.CycleStyle = func() string {
				idx := -1
				for i, name := range styleNames {
					if name == currentStyleKey {
						idx = i
						break
					}
				}
				idx++
				if idx >= len(styleNames) {
					currentStyleKey = ""
					currentResolved = nil
					ag.SetStyle(nil, nil)
					styleResult.SetArchitecture(styleconfig.Architecture{})
					styleResult.PerFileLinters.Set(nil)
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
				data := prompts.NewCodingStyleData(s.Name, wire.ConvertRules(s.Rules))
				linters := wire.LintersForStyle(s.LintCmd, styleResult.DefaultLinters)
				ag.SetStyle(data, linters)
				styleResult.SetArchitecture(s.Architecture)
				styleResult.PerFileLinters.Set(
					wire.LintersForStylePerFile(s.LintCmd, styleResult.DefaultPerFileLinters))

				currentResolved = &styleconfig.Resolved{
					Name:           s.Name,
					Rules:          s.Rules,
					LintCmd:        s.LintCmd,
					Evaluator:      s.Evaluator,
					EvaluatorModel: s.EvaluatorModel,
					Architecture:   s.Architecture,
				}

				if evaluatorActive {
					eval := wire.ForceStyleEvaluator(currentResolved, provider, llmCfg)
					ag.SetEvaluator(eval)
					if eval == nil {
						evaluatorActive = false
						app.SetEvaluatorEnabled(false)
					}
				}

				slog.Info("style: switched", "style", s.Name)
				return s.Name
			}
		}

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
				return false
			}
			if enabled {
				eval := wire.ForceStyleEvaluator(currentResolved, provider, llmCfg)
				if eval == nil {
					return false
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

	// Model switcher — first call with valid credentials constructs the agent
	// (OAuth hot-reload path); subsequent calls hot-swap the provider.
	sess.SetModelSwitcher(func(profile, modelID string) (string, error) {
		switchMu.Lock()
		defer switchMu.Unlock()

		resolved := llmconfig.ResolveProfile(llmCfg, profile)
		if resolved == nil {
			return "", fmt.Errorf("unknown profile %q", profile)
		}
		slog.Debug("llm: switch resolving", "profile", profile, "modelID", modelID, "resolvedModel", resolved.Model)
		if modelID != "" {
			resolved.Model = modelID
		}
		llmconfig.WireOAuth(resolved, pr.OAuthStore)
		llmconfig.WireStoredKey(resolved, pr.KeyStore)
		newProvider := resolved.NewProvider()
		if newProvider == nil {
			return "", fmt.Errorf("no credentials for profile %q", profile)
		}
		provider = newProvider
		if ag == nil {
			ag = buildAgent(provider)
			sess.SetAgent(ag, events)
			app.AgentPane.SetHasAgent(true)
			wireAgentHandlers()
			slog.Info("llm: agent constructed", "profile", profile, "model", resolved.Model)
		} else {
			ag.SetProvider(provider)
			if evaluatorActive && currentResolved != nil {
				eval := wire.ForceStyleEvaluator(currentResolved, provider, llmCfg)
				ag.SetEvaluator(eval)
				if eval == nil {
					evaluatorActive = false
					app.SetEvaluatorEnabled(false)
				}
			}
		}
		llmResolved = resolved
		sess.SetLLMInfo(resolved.Model, resolved.Profile)
		slog.Info("llm: switched model", "profile", profile, "model", modelID)
		displayModel := resolved.DisplayModel()
		if err := llmconfig.SaveSelection(profile, modelID); err != nil {
			return displayModel, fmt.Errorf("switched but failed to persist: %w", err)
		}
		return displayModel, nil
	})

	// Wire agent handlers if credentials were available at startup.
	if ag != nil {
		wireAgentHandlers()
	}

	slog.Debug("startup: running TUI")
	err = tuiApp.Run()
	slog.Debug("startup: TUI exited")
	appCancel()
	sess.Close()
	return err
}

// agentCompactor adapts a lazily-resolved *agent.Agent to
// codingcmd.Compactor. The agent may not exist at registry-build
// time (no LLM credentials at startup → model switcher constructs
// it on first connect), so getAgent is called per dispatch and
// returns ErrNoConversation when still nil.
type agentCompactor struct {
	getAgent func() *agent.Agent
}

// Compact resolves the active agent and forwards. Returns
// agent.ErrNoConversation when no agent has been built yet so the
// command surface shows "No conversation to compact." instead of a
// nil-pointer panic.
func (c agentCompactor) Compact(ctx context.Context) error {
	ag := c.getAgent()
	if ag == nil {
		return agent.ErrNoConversation
	}
	return ag.Compact(ctx)
}

// agentResetter adapts a lazily-resolved *agent.Agent to
// tuicmd.HistoryResetter. See agentCompactor for the lazy-resolve
// rationale.
type agentResetter struct {
	getAgent func() *agent.Agent
}

// ResetHistory resolves the active agent and forwards. Returns nil
// when no agent has been built yet — there is no history to reset,
// and surfacing an error would just block the user's pane clear.
// The companion view-side Clear still runs.
func (r agentResetter) ResetHistory(ctx context.Context) error {
	ag := r.getAgent()
	if ag == nil {
		return nil
	}
	return ag.ResetHistory(ctx)
}

// connectOpenAICmd returns a tea.Cmd that runs the OpenAI browser OAuth flow.
func connectOpenAICmd(profile string, store *oauth.Store) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		err := oauth.OpenAIBrowserFlow(ctx, store, nil)
		return nibTui.OAuthConnectResult(profile, err)
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
			return nibTui.OAuthConnectResult(profile, fmt.Errorf("request device code: %w", err))
		}

		instruction := fmt.Sprintf("Visit %s and enter code: %s", dc.VerificationURI, dc.UserCode)
		p.Send(nibTui.OAuthInstruction(profile, instruction))

		pollCtx, pollCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer pollCancel()
		err = oauth.CompleteCopilotDeviceFlow(pollCtx, store, dc)
		return nibTui.OAuthConnectResult(profile, err)
	}
}

var profileToRegistry = map[string]string{
	"chatgpt":    "openai",
	"copilot":    "github-copilot",
	"gemini":     "google",
	"minimax":    "minimax",
	"openrouter": "openrouter",
	"anthropic":  "anthropic",
}

func registryProvider(profile string) string {
	return profileToRegistry[profile]
}

func registryFilter(profile string) llm.ModelFilter {
	if profile != "chatgpt" {
		return nil
	}
	return func(m llm.RegistryModel) bool {
		return strings.Contains(m.Family, "codex") ||
			strings.HasPrefix(m.ID, "gpt-5")
	}
}

func modelsToItems(models []llm.ModelInfo, profile, defaultModel string) []nibTui.ModelSelectorItem {
	items := make([]nibTui.ModelSelectorItem, len(models))
	defaultIdx := -1
	for i, m := range models {
		items[i] = nibTui.ModelSelectorItem{ID: m.ID, Name: m.Name, Profile: profile}
		if m.ID == defaultModel {
			defaultIdx = i
		}
	}
	if defaultIdx > 0 {
		items[0], items[defaultIdx] = items[defaultIdx], items[0]
	}
	return items
}
