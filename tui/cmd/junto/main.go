package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

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
		ag := agent.New(provider, sess, events)
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
