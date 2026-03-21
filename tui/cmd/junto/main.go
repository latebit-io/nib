package main

import (
	"fmt"
	"log/slog"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/llm"
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

	// Create LLM provider from environment
	var provider llm.Provider
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
		provider = llm.NewAgentAPI(baseURL, model, apiKey)
	}

	// Create agent (nil provider = no agent, editor-only mode)
	var ag *agent.Agent
	var agentEvents <-chan agent.Event
	if provider != nil {
		events := make(chan agent.Event, 64)
		agentEvents = events
		ag = agent.New(provider, events)
	}

	sess := session.New(e, ag, agentEvents)

	app := ui.NewApp(sess)
	p := tea.NewProgram(&app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithoutSignalHandler(), // let Ctrl+C reach us as a key event
	)
	app.SetProgram(p)

	if _, err := p.Run(); err != nil {
		app.Editor.Close()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	app.Editor.Close()
}
