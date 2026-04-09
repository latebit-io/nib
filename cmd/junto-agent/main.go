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
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/headless"
	"github.com/latebit-io/junto/engine/session"
	"github.com/latebit-io/junto/engine/wire"
)

// errSetup is a sentinel wrapped into setup errors so main can distinguish
// environment/configuration failures (exit 2) from agent failures (exit 1).
var errSetup = errors.New("setup")

func main() {
	err := run()
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	if errors.Is(err, errSetup) {
		os.Exit(2)
	}
	os.Exit(1)
}

// setupErr wraps err with the errSetup sentinel so main exits with code 2.
func setupErr(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), errSetup)
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

// parseArgs parses command-line flags and positional arguments.
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

	// Detect whether stdin/stdout are terminals.
	stdinStat, err := os.Stdin.Stat()
	if err != nil {
		return setupErr("check stdin: %v", err)
	}
	stdinTTY := (stdinStat.Mode() & os.ModeCharDevice) != 0

	stdoutStat, err := os.Stdout.Stat()
	if err != nil {
		return setupErr("check stdout: %v", err)
	}
	stdoutTTY := (stdoutStat.Mode() & os.ModeCharDevice) != 0

	// Setup logging.
	if cfg.debug {
		logFile, err := os.OpenFile("/tmp/junto-agent-debug.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return setupErr("open debug log: %v", err)
		}
		defer func() {
			if err := logFile.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: close debug log: %v\n", err)
			}
		}()
		slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Resolve output format based on stdout, not stdin.
	if cfg.output != "" && cfg.output != "json" && cfg.output != "text" {
		return setupErr("invalid --output %q (expected json|text)", cfg.output)
	}
	outputJSON := cfg.output == "json" || (cfg.output == "" && !stdoutTTY)

	// Resolve project root.
	projectRoot, err := resolveProjectRoot(cfg.project)
	if err != nil {
		return setupErr("project root: %v", err)
	}

	// Read goal from stdin if not provided as an argument and stdin is piped.
	goal := cfg.goal
	if goal == "" && !stdinTTY {
		const maxGoalSize = 10 * 1024 * 1024 // 10 MB
		data, err := io.ReadAll(io.LimitReader(os.Stdin, maxGoalSize+1))
		if err != nil {
			return setupErr("read stdin: %v", err)
		}
		if len(data) > maxGoalSize {
			return setupErr("stdin goal exceeds %d bytes", maxGoalSize)
		}
		goal = strings.TrimSpace(string(data))
	}
	if goal == "" && !stdinTTY {
		return setupErr("no goal provided — pass as argument or pipe to stdin")
	}
	// goal == "" && stdinTTY → Runner handles REPL mode.

	// Create workspace.
	workspace := headless.NewDiskWorkspace(projectRoot)

	// Discover MCP tools.
	mcpResult := wire.DiscoverMCPTools(projectRoot)
	defer mcpResult.Cleanup()

	// Shared event channel.
	events := make(chan event.Event, 128)

	// Start LSP servers for language intelligence (diagnostics after edits).
	lspMgr := wire.InitLSP(projectRoot, events)
	if lspMgr != nil {
		defer func() {
			if err := lspMgr.Close(); err != nil {
				slog.Error("close LSP manager", "err", err)
			}
		}()
	}

	// Ensure demarkus binaries are installed.
	if err := wire.EnsureBinaries(projectRoot); err != nil {
		return setupErr("memory: install binaries: %v", err)
	}

	// Create LLM provider — required for headless mode.
	provider, llmCfg, llmResolved := wire.NewProvider(projectRoot)
	if provider == nil {
		hint := "set LLM_API_KEY or configure ~/.config/junto/llm.json"
		if llmResolved != nil && llmResolved.APIKeyEnv != "" {
			hint = fmt.Sprintf("set %s or configure ~/.config/junto/llm.json", llmResolved.APIKeyEnv)
		}
		return setupErr("no LLM API key — %s", hint)
	}
	slog.Debug("llm config", "profile", llmResolved.Profile, "model", llmResolved.Model)

	// Start memory server.
	mem, err := wire.StartMemory(projectRoot)
	if err != nil {
		return setupErr("memory: %v", err)
	}
	defer mem.Cleanup()

	// Resolve coding style — injected into the agent's system prompt.
	styleResult := wire.NewStyle(projectRoot)

	// Create agent in headless mode.
	opts := &agent.NewOptions{
		MemoryStore:       mem.Store,
		MemorySummary:     mem.Summary,
		Interaction:       agent.Headless,
		DistributedMemory: agent.DetectDistributedMemory(mcpResult.ServerNames),
		CodingStyle:       styleResult.AgentStyle,
	}
	if styleResult.Resolved != nil {
		opts.StyleLintCmd = styleResult.Resolved.LintCmd
		opts.StyleEvaluator = wire.NewStyleEvaluator(styleResult.Resolved, provider, llmCfg)
	}
	if lspMgr != nil {
		opts.DiagProvider = lspMgr
	}
	ag := agent.New(provider, workspace, events, opts, mcpResult.Tools...)

	// Setup cancellation via SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Stderr writer — streams status in TTY or verbose mode.
	stderr := io.Writer(io.Discard)
	if stdinTTY || cfg.verbose {
		stderr = os.Stderr
	}

	// Run the agent.
	runner := headless.NewRunner(ag, workspace, events, stderr, stdinTTY)
	result := runner.Run(ctx, goal, cfg.files)

	// Output result.
	if outputJSON {
		if err := result.WriteJSON(os.Stdout); err != nil {
			return err
		}
		if !result.Success {
			return fmt.Errorf("agent completed with errors")
		}
		return nil
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
