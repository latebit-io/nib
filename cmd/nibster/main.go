// Command nibster is the kit-boundary smoke test: a general-purpose CLI
// agent built on kit/, ai/, and stdlib. Single-shot; writes session
// findings to a demarkus memory store and maintains an index of past runs.
//
// Usage:
//
//	nibster -m "<message>"      run an agent with the given prompt
//	nibster --list              print the session index
//	nibster --show <id>         print a specific session's memory page
//
// nibster intentionally has no dependency on coding/, engine/, or tui/.
// The depguard rule in .golangci.yml enforces that boundary.
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
	"syscall"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/memory/demarkus"
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
	message string
	root    string
	list    bool
	show    string
	plugins bool
	debug   bool
}

func parseArgs() config {
	var c config
	flag.StringVar(&c.message, "m", "", "Message — runs nibster headlessly with this prompt")
	flag.StringVar(&c.root, "root", "", "Working directory for bash and demarkus root (default: cwd)")
	flag.BoolVar(&c.list, "list", false, "Print the session index and exit")
	flag.StringVar(&c.show, "show", "", "Print a specific session's memory page and exit")
	flag.BoolVar(&c.plugins, "plugins", false, "Print the wired plug-in manifest (provider, store, tools, commands) and exit")
	flag.BoolVar(&c.debug, "debug", false, "Debug logging to <user-cache-dir>/"+brand.ConfigDirName+"/nibster-debug.log")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mem, err := demarkus.Open(ctx, root)
	if err != nil {
		return setupErr("memory: %v", err)
	}
	defer func() {
		if cerr := mem.Close(); cerr != nil {
			slog.Warn("memory close", "err", cerr)
		}
	}()

	switch {
	case cfg.plugins:
		return printPlugins(root, mem.Store)
	case cfg.list:
		return listSessions(ctx, mem.Store)
	case cfg.show != "":
		return showSession(ctx, mem.Store, cfg.show)
	case cfg.message != "":
		return runAgent(ctx, root, mem.Store, cfg.message)
	default:
		flag.Usage()
		return setupErr("provide -m <message>, --list, --show <id>, or --plugins")
	}
}

// setupLogging configures slog. Debug mode writes to a per-user cache
// file (single-user trust boundary, safe against /tmp + O_TRUNC symlink
// clobber). The file handle is intentionally not closed: process
// lifetime is the only meaningful scope for a single-shot CLI.
func setupLogging(debug bool) {
	if !debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return
	}
	logPath, err := brand.DebugLogPath("nibster-debug.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: debug log path: %v — proceeding without debug log\n", err)
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: open debug log %s: %v — proceeding without debug log\n", logPath, err)
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
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
	fmt.Print(doc.Body)
	if !strings.HasSuffix(doc.Body, "\n") {
		fmt.Println()
	}
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
	fmt.Print(doc.Body)
	if !strings.HasSuffix(doc.Body, "\n") {
		fmt.Println()
	}
	return nil
}
