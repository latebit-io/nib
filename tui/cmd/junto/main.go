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
	"github.com/latebit-io/junto/engine/session"
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
func run() error {
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
	provider, llmCfg, llmResolved := wire.NewProvider(projectRoot)
	if llmResolved != nil {
		sess.SetLLMInfo(llmResolved.Model, llmResolved.Profile)
	}
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
	// Wire model listing and switching — closures capture ag, llmCfg, and llmResolved.
	if provider != nil && ag != nil {
		app.LLMProfileNames = llmCfg.ProfileNames

		app.ListModels = func(profile string) ([]ui.ModelSelectorItem, error) {
			// Resolve the profile to get its base_url and key.
			resolved := llmconfig.ResolveProfile(llmCfg, profile)
			if resolved == nil {
				// Fallback: use current provider (env-only config).
				resolved = llmResolved
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
				resolved = llmResolved
			}
			resolved.Model = modelID
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

	if _, err := p.Run(); err != nil {
		sess.Close()
		return err
	}
	sess.Close()
	return nil
}
