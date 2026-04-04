// junto-agent runs the Junto agent headlessly — same engine, same tools,
// same memory as the TUI, but without the interactive editor. Edits are
// auto-approved and written directly to disk.
//
// Usage:
//
//	junto-agent [flags] ["goal"] [files...]
//	echo "fix lint" | junto-agent --output json
//	junto-agent  # REPL mode when stdin is a TTY
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/headless"
	"github.com/latebit-io/junto/engine/session"
	"github.com/latebit-io/junto/engine/wire"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// config holds parsed command-line arguments.
type config struct {
	output  string // "json" or "text"
	project string // project root override
	verbose bool
	debug   bool
	goal    string
	files   []string
}

func parseArgs() config {
	var c config
	flag.StringVar(&c.output, "output", "", "Output format: json or text (default: text if TTY, json if piped)")
	flag.StringVar(&c.project, "project", "", "Project root directory (default: git root or cwd)")
	flag.BoolVar(&c.verbose, "verbose", false, "Stream status to stderr")
	flag.BoolVar(&c.debug, "debug", false, "Debug logging to /tmp/junto-agent-debug.log")
	flag.Parse()

	// Remaining args: [goal] [files...]
	args := flag.Args()
	if len(args) > 0 {
		c.goal = args[0]
		c.files = args[1:]
	}
	return c
}

func run() error {
	cfg := parseArgs()

	// Detect whether stdin is a terminal.
	stat, _ := os.Stdin.Stat()
	isTTY := (stat.Mode() & os.ModeCharDevice) != 0

	// Setup logging.
	if cfg.debug {
		logFile, err := os.OpenFile("/tmp/junto-agent-debug.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			defer func() { _ = logFile.Close() }()
			slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Resolve output format.
	outputJSON := cfg.output == "json" || (cfg.output == "" && !isTTY)

	// Resolve project root.
	projectRoot, err := resolveProjectRoot(cfg.project)
	if err != nil {
		return fmt.Errorf("project root: %w", err)
	}

	// Read goal from stdin if not provided as an argument and stdin is piped.
	goal := cfg.goal
	if goal == "" && !isTTY {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		goal = strings.TrimSpace(string(data))
	}
	if goal == "" && !isTTY {
		return fmt.Errorf("no goal provided — pass as argument or pipe to stdin")
	}
	// goal == "" && isTTY → Runner handles REPL mode.

	// Create workspace.
	workspace := headless.NewDiskWorkspace(projectRoot)

	// Discover MCP tools.
	mcpTools, mcpCleanup := wire.DiscoverMCPTools(projectRoot)
	defer mcpCleanup()

	// Shared event channel.
	events := make(chan event.Event, 128)

	// Start LSP servers for language intelligence (diagnostics after edits).
	lspMgr := wire.InitLSP(projectRoot, events)
	if lspMgr != nil {
		defer func() { _ = lspMgr.Close() }()
	}

	// Ensure demarkus binaries are installed.
	if err := wire.EnsureBinaries(projectRoot); err != nil {
		return fmt.Errorf("memory: install binaries: %w", err)
	}

	// Create LLM provider — required for headless mode.
	provider := wire.NewProvider()
	if provider == nil {
		return fmt.Errorf("LLM_API_KEY not set")
	}

	// Start memory server.
	mem, err := wire.StartMemory(projectRoot)
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	defer mem.Cleanup()

	// Create agent in headless mode.
	opts := &agent.NewOptions{
		MemoryStore:   mem.Store,
		MemorySummary: mem.Summary,
		Interaction:   agent.Headless,
	}
	if lspMgr != nil {
		opts.DiagProvider = lspMgr
	}
	ag := agent.New(provider, workspace, events, opts, mcpTools...)

	// Setup cancellation via SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Stderr writer — streams status in TTY or verbose mode.
	stderr := io.Writer(io.Discard)
	if isTTY || cfg.verbose {
		stderr = os.Stderr
	}

	// Run the agent.
	runner := headless.NewRunner(ag, workspace, events, stderr, isTTY)
	result := runner.Run(ctx, goal, cfg.files)

	// Output result.
	if outputJSON {
		return result.WriteJSON(os.Stdout)
	}
	return writeText(result)
}

// resolveProjectRoot determines the project root from the flag, cwd, or git walk.
func resolveProjectRoot(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", override)
		}
		return abs, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return session.ResolveProjectRoot(cwd), nil
}

// writeText writes a human-readable summary to stdout.
func writeText(r *headless.Result) error {
	if r.Summary != "" {
		fmt.Println(r.Summary)
	}
	if len(r.FilesChanged) > 0 {
		fmt.Println("\nFiles changed:")
		for _, f := range r.FilesChanged {
			fmt.Printf("  %s\n", f)
		}
	}
	if len(r.FilesCreated) > 0 {
		fmt.Println("\nFiles created:")
		for _, f := range r.FilesCreated {
			fmt.Printf("  %s\n", f)
		}
	}
	if len(r.Errors) > 0 {
		fmt.Println("\nErrors:")
		for _, e := range r.Errors {
			fmt.Printf("  %s\n", e)
		}
	}
	if !r.Success {
		return fmt.Errorf("agent completed with errors")
	}
	return nil
}
