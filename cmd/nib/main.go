// Command nib is the base agent: the default assembly of kit blocks,
// built on kit/, ai/ and stdlib only. See README.md for usage.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit/budget"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/memory/demarkus"
	"github.com/latebit-io/nib/kit/nib"
)

//go:embed prompt.md
var systemPromptTemplate string

// errSetup wraps configuration / environment failures so main can exit
// with code 2 (vs code 1 for agent runtime errors).
var errSetup = errors.New("setup")

// setupErr wraps err with the [errSetup] sentinel so main exits 2.
func setupErr(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), errSetup)
}

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

// config holds parsed command-line arguments.
type config struct {
	message  string
	root     string
	list     bool
	show     string
	plugins  bool
	trustMCP bool
	mcpLogin string
	with     string
	without  string
	approve  bool
	budget   int
	debug    bool
}

// interactive reports whether no flag asked for anything else: the
// default is a session in the terminal.
func (c config) interactive() bool {
	return c.message == "" && !c.list && c.show == "" && !c.plugins && !c.trustMCP && c.mcpLogin == ""
}

// choices merges configuration, environment and flags; the later one
// decides. Approve mode needs a session, since a single run cannot ask.
func (c config) choices() (choices, error) {
	chosen := choices{switches: configuredSwitches().Then(c.switches()), approve: c.approve}
	if c.approve && !c.interactive() {
		return chosen, errors.New("--approve needs a session: a single run has nobody to ask")
	}
	// The flag decides, so a bad environment value does not matter then.
	budgetCap := c.budget
	if budgetCap == 0 {
		var err error
		if budgetCap, err = budget.ParseEnvCap(os.Getenv(brand.EnvKeyTaskTokenBudget)); err != nil {
			return chosen, fmt.Errorf("%s: %w", brand.EnvKeyTaskTokenBudget, err)
		}
	}
	chosen.limits = nib.Limits{
		TokenBudget: budgetCap,
		NoTrimming:  os.Getenv(brand.EnvKeyToolOutputCapDisabled) != "",
	}
	return chosen, nil
}

// switches returns the extensions this run's flags switch on and off.
func (c config) switches() extension.Switches {
	list := func(s string) []string {
		var names []string
		for name := range strings.SplitSeq(s, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
		return names
	}
	return extension.Switches{Enable: list(c.with), Disable: list(c.without)}
}

func parseArgs() config {
	var c config
	flag.StringVar(&c.message, "m", "", "Message: runs nib once with this prompt instead of starting a session")
	flag.StringVar(&c.root, "root", "", "Working directory for bash and demarkus root (default: cwd)")
	flag.BoolVar(&c.list, "list", false, "Print the session index and exit")
	flag.StringVar(&c.show, "show", "", "Print a specific session's memory page and exit")
	flag.BoolVar(&c.plugins, "plugins", false, "Print the wired plug-in manifest (provider, store, tools, commands) and exit")
	flag.StringVar(&c.with, "with", "", "Extensions to switch on for this run, comma-separated")
	flag.StringVar(&c.without, "without", "", "Extensions to switch off for this run, comma-separated")
	flag.BoolVar(&c.approve, "approve", false, "Start the session in approve mode: ask before each command and file change")
	flag.IntVar(&c.budget, "budget", 0, "End the session's runs once this many tokens are used (default: "+brand.EnvKeyTaskTokenBudget+", else none)")
	flag.BoolVar(&c.trustMCP, "trust-mcp", false, "Allow the MCP servers this project's .mcp.json names, and exit")
	flag.StringVar(&c.mcpLogin, "mcp-login", "", "Sign in to the named HTTP MCP server in the browser, keep the sign-in, and exit")
	flag.BoolVar(&c.debug, "debug", false, "Debug logging to <user-cache-dir>/"+brand.ConfigDirName+"/nib-debug.log")
	flag.Parse()
	return c
}

func run() error {
	cfg := parseArgs()

	setupLogging(cfg.debug)

	root, err := resolveRoot(cfg.root)
	if err != nil {
		return setupErr("resolve root: %v", err)
	}

	// Needs no memory store and no model: do it before either starts.
	if cfg.trustMCP {
		return trustMCP(root)
	}
	if cfg.mcpLogin != "" {
		return mcpLogin(root, cfg.mcpLogin)
	}

	// An interactive session takes Ctrl-C itself: it stops a run
	// without ending the session.
	signals := []os.Signal{syscall.SIGTERM}
	var interrupt chan os.Signal
	if cfg.interactive() {
		interrupt = make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		defer signal.Stop(interrupt)
	} else {
		signals = append(signals, os.Interrupt)
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()
	// Nothing reads interrupt until the session runs, so until then
	// Ctrl-C cancels the startup instead.
	startCtx, started := cancelOn(ctx, interrupt)
	defer started()

	mem, err := demarkus.Open(startCtx, root)
	if err != nil {
		return setupErr("memory: %v", err)
	}
	defer func() {
		if cerr := mem.Close(); cerr != nil {
			slog.Warn("memory close", "err", cerr)
		}
	}()

	// Reading past sessions runs no agent, so run settings cannot stop it.
	switch {
	case cfg.list:
		return listSessions(ctx, mem.Store)
	case cfg.show != "":
		return showSession(ctx, mem.Store, cfg.show)
	}
	chosen, err := cfg.choices()
	if err != nil {
		return setupErr("%v", err)
	}
	switch {
	case cfg.plugins:
		return printPlugins(ctx, root, mem.Store, chosen)
	case cfg.interactive():
		return runInteractive(ctx, startCtx, started, root, mem.Store, chosen, interrupt)
	default:
		return runAgent(ctx, root, mem.Store, cfg.message, chosen)
	}
}

// cancelOn returns a context that the first value on interrupt
// cancels, until release is called. After that interrupt belongs to
// whoever reads it next, and the context lives as long as ctx.
func cancelOn(ctx context.Context, interrupt <-chan os.Signal) (startCtx context.Context, release func()) {
	startCtx, cancel := context.WithCancel(ctx)
	released, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		select {
		case <-interrupt:
			cancel()
		case <-released:
		case <-ctx.Done():
		}
	}()
	// Waits for the goroutine: a signal sent after release returns must
	// find nobody else reading the channel.
	return startCtx, sync.OnceFunc(func() {
		close(released)
		<-gone
	})
}

// warn reports a problem the run survives. slog alone would hide it:
// without --debug it is discarded.
func warn(what string, err error) {
	fmt.Fprintf(os.Stderr, "warning: %s: %v\n", what, err)
	slog.Warn(what, "err", err)
}

// setupLogging configures slog. Debug mode writes to a per-user cache
// file (single-user trust boundary, safe against /tmp + O_TRUNC symlink
// clobber). Losing the debug log is not a reason to refuse to run: open
// failures warn and continue. The file handle is intentionally not
// closed: process lifetime is the only meaningful scope for a
// single-shot CLI.
func setupLogging(debug bool) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !debug {
		return
	}
	f, err := openDebugLog("nib-debug.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — proceeding without debug log\n", err)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
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

// resolveRoot returns the working directory for bash and demarkus. The
// override flag takes precedence when set; otherwise cwd.
func resolveRoot(override string) (string, error) {
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
	return os.Getwd()
}

// credentialHint produces a "set X or configure Y" message tailored to
// the resolved profile so users see the actual env var name when one is
// configured, not just the LLM_API_KEY default.
func credentialHint(r *llmconfig.Resolved) string {
	cfgPath := llmconfig.GlobalConfigPath()
	if cfgPath == "" {
		cfgPath = "<user-config-dir>/" + brand.ConfigDirName + "/llm.json"
	}
	if r != nil && r.APIKeyEnv != "" {
		return fmt.Sprintf("set %s or configure %s", r.APIKeyEnv, cfgPath)
	}
	return fmt.Sprintf("set LLM_API_KEY or configure %s", cfgPath)
}

// buildSystemPrompt expands the embedded prompt template with the
// session ID, so the agent knows the exact path to write to.
func buildSystemPrompt(sessionID string) string {
	return strings.ReplaceAll(systemPromptTemplate, "{{SESSION_ID}}", sessionID)
}

// listSessions prints /nibster/index.md to stdout, or a placeholder if
// the index does not exist yet.
func listSessions(ctx context.Context, store memory.Store) error {
	doc, err := store.Fetch(ctx, indexPath)
	if errors.Is(err, memory.ErrNotFound) {
		fmt.Println("(no sessions yet)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}
	printDoc(doc.Body)
	return nil
}

// showSession prints /nibster/sessions/<id>.md to stdout. Validates id
// against the canonical session-ID shape so a path-like value (../foo,
// absolute paths, embedded slashes) cannot escape sessionsDir and read
// arbitrary documents from the store.
func showSession(ctx context.Context, store memory.Store, id string) error {
	if !validSessionID(id) {
		return setupErr("invalid session id %q (expected <YYYY-MM-DD-HHMMSS>-<slug>-<hex>)", id)
	}
	path := sessionsDir + "/" + id + ".md"
	doc, err := store.Fetch(ctx, path)
	if errors.Is(err, memory.ErrNotFound) {
		return fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}
	printDoc(doc.Body)
	return nil
}

// printDoc writes a memory document body to stdout, ensuring it ends
// with a newline so the shell prompt never lands mid-line.
func printDoc(body string) {
	fmt.Print(body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
}
