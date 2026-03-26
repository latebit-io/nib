package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/mcp"
	"github.com/latebit-io/junto/engine/session"
	"github.com/latebit-io/junto/tui/internal/ui"
)

func main() {
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
		logFile, err := os.OpenFile("/tmp/junto-debug.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			defer func() { _ = logFile.Close() }()
			slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
	} else {
		// Discard all logs — slog defaults to stderr which corrupts the alt-screen TUI.
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	var buf *buffer.Buffer
	if filePath != "" {
		var err error
		buf, err = buffer.NewFromFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	} else {
		buf = buffer.New()
	}

	e := editor.New(buf)

	// Determine project root: walk up from file path (or cwd) to find .git.
	startDir, _ := os.Getwd()
	if filePath != "" {
		if absPath, err := filepath.Abs(filePath); err == nil {
			startDir = filepath.Dir(absPath)
		}
	}
	projectRoot := startDir
	for dir := startDir; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			projectRoot = dir
			break
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}

	// Create session first (editor-only mode) — it serves as the agent's Workspace.
	sess := session.New(e, projectRoot)

	// Discover MCP tools from .project/mcp.json or JUNTO_MCP env var.
	mcpTools, mcpCleanup := discoverMCPTools(projectRoot)
	defer mcpCleanup()

	// Create LLM provider and agent from environment
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey != "" {
		baseURL := os.Getenv("LLM_BASE_URL")
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
		model := os.Getenv("LLM_MODEL")
		if model == "" {
			model = "google/gemini-2.5-flash"
		}
		provider := llm.NewAgentAPI(baseURL, model, apiKey)
		events := make(chan agent.Event, 64)
		ag := agent.New(provider, sess, events, projectRoot, mcpTools...)
		sess.SetAgent(ag, events)
	}

	app := ui.NewApp(sess)
	app.ProjectRoot = projectRoot

	// Agent typing speed (words per minute)
	if wpmStr := os.Getenv("JUNTO_TYPING_WPM"); wpmStr != "" {
		if wpm, err := strconv.Atoi(wpmStr); err == nil && wpm > 0 {
			app.Editor.TypingWPM = wpm
		}
	}
	p := tea.NewProgram(&app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithoutSignalHandler(), // let Ctrl+C reach us as a key event
	)
	app.SetProgram(p)

	if _, err := p.Run(); err != nil {
		sess.Close()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	sess.Close()
}

// mcpServerConfig describes one MCP server in .project/mcp.json.
type mcpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// mcpConfig is the top-level structure of .mcp.json (same format as Claude Code).
type mcpConfig struct {
	Servers map[string]mcpServerConfig `json:"mcpServers"`
}

// discoverMCPTools connects to MCP servers and returns their tools as agent.Tool adapters.
// Returns a cleanup function that closes all MCP clients.
func discoverMCPTools(projectRoot string) ([]agent.Tool, func()) {
	var tools []agent.Tool
	var clients []*mcp.Client

	cleanup := func() {
		for _, c := range clients {
			_ = c.Close() // best-effort cleanup on shutdown
		}
	}

	configs := loadMCPConfigs(projectRoot)
	if len(configs) == 0 {
		return nil, cleanup
	}

	for name, cfg := range configs {
		// Split command into program + args if needed.
		cmdParts := strings.Fields(cfg.Command)
		if len(cmdParts) == 0 {
			slog.Warn("mcp: empty command for server", "name", name)
			continue
		}
		program := cmdParts[0]
		args := append(cmdParts[1:], cfg.Args...)

		// Build environment.
		var env []string
		if len(cfg.Env) > 0 {
			env = os.Environ()
			for k, v := range cfg.Env {
				env = append(env, k+"="+v)
			}
		}

		client, err := mcp.NewStdioClient(program, args, env)
		if err != nil {
			slog.Warn("mcp: failed to start server", "name", name, "err", err)
			continue
		}
		clients = append(clients, client)

		// Each server gets its own timeout so a slow server doesn't starve others.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		if err := client.Initialize(ctx); err != nil {
			cancel()
			slog.Warn("mcp: failed to initialize server", "name", name, "err", err)
			continue
		}

		serverTools, err := client.ListTools(ctx)
		cancel()
		if err != nil {
			slog.Warn("mcp: failed to list tools", "name", name, "err", err)
			continue
		}

		for _, info := range serverTools {
			tools = append(tools, agent.NewMCPToolAdapter(client, info))
			slog.Debug("mcp: registered tool", "server", name, "tool", info.Name)
		}
		slog.Info("mcp: connected", "server", name, "tools", len(serverTools))
	}

	return tools, cleanup
}

// loadMCPConfigs reads MCP server configurations from .project/mcp.json
// or the JUNTO_MCP environment variable.
func loadMCPConfigs(projectRoot string) map[string]mcpServerConfig {
	// Try .mcp.json at project root (same location as Claude Code).
	configPath := filepath.Join(projectRoot, ".mcp.json")
	data, err := os.ReadFile(configPath)
	if err == nil {
		var cfg mcpConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			slog.Warn("mcp: invalid config", "path", configPath, "err", err)
		} else if len(cfg.Servers) > 0 {
			return cfg.Servers
		}
	}

	// Fall back to JUNTO_MCP env var: "name=command arg1 arg2"
	// Multiple servers separated by semicolons.
	mcpEnv := os.Getenv("JUNTO_MCP")
	if mcpEnv == "" {
		return nil
	}

	configs := make(map[string]mcpServerConfig)
	for _, entry := range strings.Split(mcpEnv, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, cmd, found := strings.Cut(entry, "=")
		if !found || cmd == "" {
			slog.Warn("mcp: invalid JUNTO_MCP entry", "entry", entry)
			continue
		}
		configs[name] = mcpServerConfig{Command: cmd}
	}
	return configs
}
