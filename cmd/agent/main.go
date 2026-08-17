// Command nib-agent runs the coding agent headlessly — same engine,
// same tools, same memory as the TUI, but without the interactive
// editor. Edits are auto-approved and written directly to disk.
//
// Usage:
//
//	nib-agent [flags] ["goal"] [files...]
//	echo "fix lint" | nib-agent --output json
//	nib-agent  # REPL mode when stdin is a TTY
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

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	codingmemory "github.com/latebit-io/nib/coding/memory"
	"github.com/latebit-io/nib/coding/runconfig"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/coding/wire"
	"github.com/latebit-io/nib/kit/budget"
	"github.com/latebit-io/nib/kit/cmdallow"
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
	flag.BoolVar(&c.debug, "debug", false, "Debug logging to <user-cache-dir>/"+brand.ConfigDirName+"/agent-debug.log")
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

	// Setup logging. Debug mode writes to the per-user cache dir (a
	// single-user trust boundary, safe against /tmp + O_TRUNC symlink
	// clobber). Losing the debug log is not a reason to refuse to run —
	// headless included — so open failures warn and continue without it.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if cfg.debug {
		logFile, err := openDebugLog("agent-debug.log")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v — proceeding without debug log\n", err)
		} else {
			defer func() {
				if err := logFile.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "warning: close debug log: %v\n", err)
				}
			}()
			slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
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

	// Setup cancellation via SIGINT/SIGTERM. Established before
	// StartMemory so a Ctrl-C during the seed RPC unwinds cleanly
	// instead of being held off until the agent run begins.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Ensure demarkus binaries are installed.
	if err := wire.EnsureBinaries(ctx, projectRoot); err != nil {
		return setupErr("memory: install binaries: %v", err)
	}

	// Create LLM provider — required for headless mode.
	pr := wire.NewProvider(projectRoot)
	provider, llmResolved := pr.Provider, pr.Resolved
	if provider == nil {
		// Use the resolved global config path so the hint shows the
		// correct platform-specific location (~/.config/<brand>/llm.json
		// on Linux, ~/Library/Application Support/<brand>/llm.json on
		// macOS). Falls back to a placeholder if UserConfigDir fails.
		cfgPath := llmconfig.GlobalConfigPath()
		if cfgPath == "" {
			cfgPath = "<user-config-dir>/" + brand.ConfigDirName + "/llm.json"
		}
		hint := fmt.Sprintf("set LLM_API_KEY or configure %s", cfgPath)
		if llmResolved != nil && llmResolved.APIKeyEnv != "" {
			hint = fmt.Sprintf("set %s or configure %s", llmResolved.APIKeyEnv, cfgPath)
		}
		return setupErr("no LLM API key — %s", hint)
	}
	slog.Debug("llm config", "profile", llmResolved.Profile, "model", llmResolved.Model)

	// Start memory server.
	mem, err := wire.StartMemory(ctx, projectRoot)
	if err != nil {
		return setupErr("memory: %v", err)
	}
	defer mem.Cleanup()

	// Auto-detect post-task linters (golangci-lint / luacheck / etc).
	linters := wire.NewLinters(projectRoot)

	// Resolve smoke-run config. In headless / CI mode this is the
	// primary safety net that catches "compiles clean but won't launch"
	// regressions before the agent reports success.
	smokeCfg := runconfig.Load(projectRoot)
	if smokeCfg.Skipped {
		slog.Info("smoke: skipped", "reason", smokeCfg.SkipReason)
	} else {
		slog.Info("smoke: configured", "command", smokeCfg.Command, "source", smokeCfg.Source)
	}

	// Repo-carried instruction files (AGENTS.md / CLAUDE.md); a missing
	// file is the common case, an unreadable/oversize one degrades to no
	// injection rather than blocking a headless run.
	contextFiles, cfErr := wire.LoadContextFiles(projectRoot)
	if cfErr != nil {
		slog.Warn("context file skipped", "err", cfErr)
	}

	// Create agent in headless mode.
	opts := &agent.NewOptions{
		MemoryStore:       mem.Store,
		MemorySummary:     mem.Summary,
		Interaction:       agent.Headless,
		ContextFiles:      contextFiles,
		DistributedMemory: codingmemory.DetectDistributedMemory(mcpResult.ServerNames),
		Linters:           linters.PostTask,
		SmokeConfig:       smokeCfg,
		// Headless defaults off: the runner auto-approves every
		// proposal, so arming it only adds a per-command status trace.
		// NIB_BASH_APPROVAL=1 opts in.
		ApproveBashCommands: brand.BashApprovalEnabled(false),
		// The allowlist still applies when opted in: allowlisted
		// commands skip even the status trace.
		BashAllowlist: cmdallow.Load(projectRoot),
	}
	// A malformed override is a hard error rather than a silent
	// fall-through to disabled — matters most here in headless / CI mode
	// where no human is watching the run burn tokens uncapped.
	taskTokenBudgetCap, err := budget.ParseEnvCap(os.Getenv(brand.EnvKeyTaskTokenBudget))
	if err != nil {
		return setupErr("%v", err)
	}
	opts.TaskTokenBudget = taskTokenBudgetCap
	if lspMgr != nil {
		opts.DiagProvider = lspMgr
	}
	// nil when validators are disabled via env.
	opts.ValidationPipeline = wire.NewValidationPipeline()
	ag := agent.New(provider, workspace, opts, mcpResult.Tools...)
	// Forward bus-published agent events into the shared `events` chan
	// the headless [Runner] reads.
	if err := wire.ForwardEvents(ctx, ag, events); err != nil {
		return setupErr("%v", err)
	}

	// Stderr writer — streams status in TTY or verbose mode.
	stderr := io.Writer(io.Discard)
	if stdinTTY || cfg.verbose {
		stderr = os.Stderr
	}

	// Run the agent.
	runner := headless.NewRunner(ag, workspace, events, stderr, stdinTTY)
	result := runner.Run(ctx, goal, cfg.files)

	// Drain the events channel so the forwarder can exit cleanly.
	// On context cancellation the runner returns before AgentDone,
	// leaving pending sends that would block without a consumer.
	drainDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-drainDone:
				return
			case <-events:
			}
		}
	}()
	ag.Close()
	close(drainDone)

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
	return writeText(os.Stdout, result)
}

// openDebugLog opens (truncating) the brand debug-log file named name
// under the per-user cache dir. The caller owns the returned file.
func openDebugLog(name string) (*os.File, error) {
	logPath, err := brand.DebugLogPath(name)
	if err != nil {
		return nil, fmt.Errorf("debug log path: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open debug log %s: %w", logPath, err)
	}
	return f, nil
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

// writeText writes a human-readable summary of r to w. Returns an
// error when the run did not succeed so main exits non-zero.
func writeText(w io.Writer, r *headless.Result) error {
	var sb strings.Builder
	if r.Summary != "" {
		sb.WriteString(r.Summary + "\n")
	}
	writeList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		sb.WriteString("\n" + title + ":\n")
		for _, it := range items {
			sb.WriteString("  " + it + "\n")
		}
	}
	writeList("Files changed", r.FilesChanged)
	writeList("Files created", r.FilesCreated)
	writeList("Errors", r.Errors)
	if _, err := io.WriteString(w, sb.String()); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if !r.Success {
		return fmt.Errorf("agent completed with errors")
	}
	return nil
}
