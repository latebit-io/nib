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
	"github.com/latebit-io/nib/cmd/nib-code/defaults"
	"github.com/latebit-io/nib/coding/agent"
	codingcmd "github.com/latebit-io/nib/coding/command"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	codingmemory "github.com/latebit-io/nib/coding/memory"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/coding/subagent"
	"github.com/latebit-io/nib/coding/wire"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/capture/demarkus"
	"github.com/latebit-io/nib/engine/highlight"
	"github.com/latebit-io/nib/engine/openfile"
	"github.com/latebit-io/nib/engine/runconfig"
	"github.com/latebit-io/nib/engine/validate"
	"github.com/latebit-io/nib/engine/validate/goparse"
	"github.com/latebit-io/nib/engine/validate/treesitter"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/budget"
	kitcmd "github.com/latebit-io/nib/kit/command"
	cmdloader "github.com/latebit-io/nib/kit/command/loader"
	"github.com/latebit-io/nib/kit/pluginstore"
	"github.com/latebit-io/nib/kit/skill"
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
	// Parse args: [--debug] [--plugins] [file]
	args := os.Args[1:]
	debug := false
	pluginsOnly := false
	var filePath string
	for _, a := range args {
		switch a {
		case "--debug":
			debug = true
		case "--plugins":
			pluginsOnly = true
		default:
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

	of := openfile.New(buf)

	sess := session.New(of, projectRoot)
	sess.SetContext(appCtx)

	// Construct the managed plugin store. Non-fatal on failure: nib runs
	// fine without plugins, so we log and proceed with an empty set.
	var pluginStore *pluginstore.Store
	var activePlugins []pluginstore.ActivePlugin
	if pdir, perr := brand.PluginsDir(); perr != nil {
		slog.Warn("plugins: cannot resolve store dir; plugins disabled", "err", perr)
	} else if ps, perr := pluginstore.New(pdir); perr != nil {
		slog.Warn("plugins: store init failed; plugins disabled", "err", perr)
	} else {
		pluginStore = ps
		activePlugins = ps.ActivePlugins()
	}

	// Discover MCP tools from .mcp.json or the brand-prefixed MCP env var,
	// then add MCP servers contributed by enabled plugins' converted
	// configs (namespaced by plugin id to avoid cross-plugin collisions).
	mcpResult := wire.DiscoverMCPTools(projectRoot)
	if len(activePlugins) > 0 {
		var sources []wire.MCPConfigSource
		for _, p := range activePlugins {
			sources = append(sources, wire.MCPConfigSource{Prefix: p.ID, Path: p.MCPConfigPath})
		}
		mcpResult = combineMCPResults(mcpResult, wire.DiscoverMCPToolsFromSources(sources))
	}
	defer mcpResult.Cleanup()

	// Discover model-invoked skills from project-local (.project/skills),
	// user-global (<UserConfigDir>/nib/skills), and enabled plugins'
	// converted skills (lowest precedence). Pure-prompt skills become
	// tools; script-bearing skills are refused (logged) until the
	// bash-approval surface exists.
	var pluginSkills []skill.PluginSkillSource
	for _, p := range activePlugins {
		pluginSkills = append(pluginSkills, skill.PluginSkillSource{ID: p.ID, Dir: p.SkillsDir})
	}
	// Trust gate: a plugin's shell-bearing skills load only when the user
	// has trusted that plugin (via `/plugin trust`). Backed by the store's
	// persisted grant; nil store ⇒ nothing trusted.
	trusted := func(pluginID string) bool {
		if pluginStore == nil {
			return false
		}
		p, ok := pluginStore.Get(pluginID)
		return ok && p.Trusted
	}
	skillResult, skillErr := skill.DiscoverWithPlugins(projectRoot, pluginSkills, trusted)
	if skillErr != nil {
		slog.Warn("skills: some skills failed to load", "err", skillErr)
	}
	for _, s := range skillResult.Loaded {
		slog.Info("skills: loaded", "skill", s.Name, "source", s.Source)
	}

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

	// Resolve the per-task token budget once so every rebuilt agent, the
	// subagent spawner, and the TUI status indicator agree on the same
	// armed cap. A malformed override is a hard error rather than a silent
	// fall-through to disabled — the var is only set with intent to change
	// the cap.
	taskTokenBudgetCap, err := budget.ParseEnvCap(os.Getenv(brand.EnvKeyTaskTokenBudget))
	if err != nil {
		return err
	}

	// Subagents: discover definitions (project/global/enabled-plugin),
	// trust-gate plugin agents, and adapt each into an `agent_<name>`
	// spawn tool. Children run headless against the project root,
	// inheriting the parent's MCP + skill tools filtered by the
	// definition's grants. A definition's model override is resolved by
	// cloning the active profile with the requested model id.
	var subagentProviderFor func(string) (llm.Provider, error)
	if llmResolved != nil {
		baseResolved := *llmResolved
		subagentProviderFor = func(model string) (llm.Provider, error) {
			r := baseResolved
			r.Model = model
			return r.NewProvider(), nil
		}
	}
	// Track the live provider so subagents stay in sync with credential /
	// model switches instead of capturing the (possibly nil) startup one.
	// buildAgent updates this on every (re)build via setSubagentProvider.
	var (
		subagentProviderMu sync.Mutex
		subagentProvider   = provider
	)
	setSubagentProvider := func(p llm.Provider) {
		subagentProviderMu.Lock()
		subagentProvider = p
		subagentProviderMu.Unlock()
	}
	spawner := subagent.New(subagent.Options{
		Workspace: headless.NewDiskWorkspace(projectRoot),
		Provider: func() llm.Provider {
			subagentProviderMu.Lock()
			defer subagentProviderMu.Unlock()
			return subagentProvider
		},
		ProviderFor: subagentProviderFor,
		BaseTools:   append(append([]agent.Tool{}, mcpResult.Tools...), skillResult.Tools...),
		TokenBudget: taskTokenBudgetCap,
		// Surface child-agent progress (tool calls, name-prefixed) in the
		// parent's transcript. Best-effort: drop on a full event buffer
		// rather than stall the subagent run.
		OnEvent: func(ev event.Event) {
			select {
			case events <- ev:
			default:
			}
		},
	})
	var pluginAgentSources []subagent.PluginAgentSource
	for _, p := range activePlugins {
		pluginAgentSources = append(pluginAgentSources, subagent.PluginAgentSource{ID: p.ID, Dir: p.AgentsDir})
	}
	subagentResult, subagentErr := subagent.Discover(projectRoot, pluginAgentSources, trusted, spawner)
	if subagentErr != nil {
		slog.Warn("subagents: some definitions failed to load", "err", subagentErr)
	}
	for _, d := range subagentResult.Loaded {
		slog.Info("subagent: loaded", "agent", d.Name, "source", d.Source)
	}
	// context:fork skills become spawn tools (skill_<name>) backed by the
	// same spawner, running their body as an isolated child agent.
	var forkSkillTools []agent.Tool
	for _, s := range skillResult.Forking {
		forkSkillTools = append(forkSkillTools, subagent.AdaptForkSkill(s, spawner))
		slog.Info("skill: loaded as fork (subagent)", "skill", s.Name, "source", s.Source)
	}

	linters := wire.NewLinters(projectRoot)

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
	var tuiApp *nibTui.App
	var switchMu sync.Mutex

	// flushDirtyBuffersFn routes the agent's pre-tool-dispatch autosave
	// step back through the TUI's Update goroutine via
	// [nibTui.App.FlushDirtyBuffers]. Calling [session.Session.
	// SaveDirtyBuffers] directly from the agent's tool-dispatch
	// goroutine would race the Update goroutine's keystroke-driven
	// buffer mutations — [engine/buffer.Buffer] has no internal lock.
	//
	// The closure captures [tuiApp] by name; the variable is non-nil
	// by the time any agent run starts (TUI is constructed below before
	// [tuiApp.Run] is called). The nil check is defensive for the
	// startup-race window in which the agent is constructed but the
	// TUI is not yet wired — no agent run can fire in that window, so
	// returning (nil, nil) is a safe no-op there.
	flushDirtyBuffersFn := func(ctx context.Context) ([]string, error) {
		if tuiApp == nil {
			return nil, nil
		}
		return tuiApp.FlushDirtyBuffers(ctx)
	}

	buildAgent := func(p llm.Provider) *agent.Agent {
		// Keep subagents pointed at the provider the parent is now using.
		setSubagentProvider(p)
		opts := &agent.NewOptions{
			MemoryStore:       mem.Store,
			MemorySummary:     mem.Summary,
			DistributedMemory: distributed,
			Linters:           linters.PostTask,
			Terse:             true,
			SmokeConfig:       smokeCfg,
			FlushDirtyBuffers: flushDirtyBuffersFn,
			TaskTokenBudget:   taskTokenBudgetCap,
		}
		if lspMgr != nil {
			opts.DiagProvider = lspMgr
		}
		if os.Getenv(brand.EnvKeyValidatorsDisabled) == "" {
			opts.ValidationPipeline = validate.NewPipeline(
				goparse.Validator{},
				treesitter.New(highlight.LanguageFor),
			)
		}
		extraTools := append(append([]agent.Tool{}, mcpResult.Tools...), skillResult.Tools...)
		extraTools = append(extraTools, subagentResult.Tools...)
		extraTools = append(extraTools, forkSkillTools...)
		ag := agent.New(p, sess, opts, extraTools...)
		// Forward bus-published agent events into the shared `events`
		// chan that LSP also writes to. The TUI reads this single
		// merged stream via [session.Session.Events]. The forwarder
		// goroutine exits naturally when [agent.Agent.Close] closes
		// the bus (which closes the subscription's inbox).
		sub, err := ag.Subscribe(agent.SubscribeOptions{BufferSize: 128})
		if err != nil {
			panic(fmt.Sprintf("nib-code: agent.Subscribe: %v", err))
		}
		go func() {
			for ev := range sub.Events() {
				select {
				case events <- ev:
				case <-appCtx.Done():
					return
				}
			}
		}()
		return ag
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

	// Construct the TUI via the facade. tuiApp is forward-declared
	// above so the agent's FlushDirtyBuffers callback (set inside
	// buildAgent) can route through it via closure capture.
	tuiApp = nibTui.New(nibTui.Config{
		Session: sess,
		Events:  events,
		// Resolve the live agent at shutdown. `ag` is captured by
		// reference: it is nil at startup without LLM credentials and
		// is built lazily by the model switcher on first connect. Return
		// a true nil interface (not a typed-nil *agent.Agent) when nil so
		// the TUI's shutdown skips Close instead of panicking on a nil
		// receiver.
		Agent: func() kit.AgentLifecycle {
			if ag == nil {
				return nil
			}
			return ag
		},
		LLM:                llmCallbacks,
		TaskTokenBudget:    taskTokenBudgetCap,
		HighlighterFactory: highlight.NewHighlighter,
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
	// Cold-start seed: when a project has no .project/commands/
	// directory yet, materialize the binary's bundled starter
	// templates so first-launch users see the surface immediately.
	// Idempotent at the dir level — once the directory exists
	// (even empty), seeding never runs again. Failures here are
	// logged but never abort startup; the rest of nib-code is
	// useful even if the seed write failed (full-disk, permissions,
	// etc.).
	projectCommandsDir := filepath.Join(projectRoot, ".project", "commands")
	if err := defaults.Seed(projectCommandsDir); err != nil {
		slog.Warn("commands: seed defaults failed", "dir", projectCommandsDir, "err", err)
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
	loadCommandDir(projectCommandsDir, kitcmd.SourceProject)
	if userCfgDir, err := os.UserConfigDir(); err == nil {
		loadCommandDir(filepath.Join(userCfgDir, brand.ConfigDirName, "commands"), kitcmd.SourceGlobal)
	} else {
		slog.Debug("commands: skipping global dir, UserConfigDir unavailable", "err", err)
	}
	// Commands imported from enabled plugins (lower precedence than the
	// user's own project/global markdown).
	for _, p := range activePlugins {
		loadCommandDir(p.CommandsDir, kitcmd.SourcePlugin)
	}

	// /plugin — manage imported Claude Code plugins. Registered only when
	// the store initialized; without it the command would have no backend.
	if pluginStore != nil {
		if err := cmdRegistry.Register(codingcmd.NewPlugin(pluginStore)); err != nil {
			return fmt.Errorf("register /plugin: %w", err)
		}
	}

	// /new-command — scaffold a new markdown command. Registered
	// after the markdown loader runs so its CommandLookup probe
	// sees every already-loaded command and refuses shadowing.
	if err := cmdRegistry.Register(codingcmd.NewNewCommand(projectCommandsDir, cmdRegistry)); err != nil {
		return fmt.Errorf("register /new-command: %w", err)
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
	// /capabilities — the in-TUI `--plugins` view. The thunk re-renders
	// the manifest on each invocation (ag/llmResolved captured by ref)
	// so a provider hot-swap is reflected.
	if err := cmdRegistry.Register(newCapabilitiesCommand(func() string {
		return buildPluginsManifest(ag, cmdRegistry, mem.Store, llmResolved, skillResult)
	})); err != nil {
		return fmt.Errorf("register /capabilities: %w", err)
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

	// wireAgentHandlers installs the UI callbacks that require a live
	// agent through the typed nibTui.AgentCallbacks / CodingCallbacks
	// surface. Called once — on startup (when credentials exist) or
	// on the first successful connect via the model switcher.
	//
	// The two Set*Callbacks calls dispatch through tea.Program.Send,
	// which serializes the field writes into the Bubble Tea Update
	// goroutine and so stays race-free regardless of which caller
	// goroutine invokes wireAgentHandlers (startup goroutine pre-Run;
	// Update goroutine via model switcher post-Run).
	wireAgentHandlers := func() {
		tuiApp.SetAgentCallbacks(nibTui.AgentCallbacks{
			ToggleTerse: func(enabled bool) bool {
				ag.SetTerse(enabled)
				if enabled {
					slog.Info("terse mode: enabled")
				} else {
					slog.Info("terse mode: disabled")
				}
				return enabled
			},
			InitialTerse: true,
		})

		tuiApp.SetCodingCallbacks(nibTui.CodingCallbacks{})
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

	if pluginsOnly {
		fmt.Print(buildPluginsManifest(ag, cmdRegistry, mem.Store, llmResolved, skillResult))
		return nil
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

// combineMCPResults merges two MCP discovery results into one, chaining
// their cleanups so both sets of clients close on shutdown. Used to fold
// plugin-contributed MCP servers into the project's discovery result.
func combineMCPResults(a, b wire.MCPResult) wire.MCPResult {
	return wire.MCPResult{
		Tools:       append(append([]agent.Tool{}, a.Tools...), b.Tools...),
		ServerNames: append(append([]string{}, a.ServerNames...), b.ServerNames...),
		Cleanup: func() {
			a.Cleanup()
			b.Cleanup()
		},
	}
}
