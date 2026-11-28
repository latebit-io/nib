package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit/a2a"
	"github.com/latebit-io/nib/kit/memory/demarkus"
	"github.com/latebit-io/nib/kit/nib"
)

// defaultServeAddr is where `nib serve` listens unless told otherwise.
const defaultServeAddr = "127.0.0.1:8765"

// shutdownGrace bounds how long a stopping server waits for open requests.
const shutdownGrace = 5 * time.Second

// a2aAgents is the person's list of remote agents. A list that cannot
// be read is reported and the session goes on without it.
func a2aAgents() a2a.Config {
	path, err := a2a.DefaultConfigPath()
	if err != nil {
		warn("A2A agents", err)
	}
	cfg, err := a2a.LoadConfig(path, os.Getenv(a2a.EnvKey))
	if err != nil {
		warn("A2A agents", err)
	}
	return cfg
}

// serveUsage lists what `nib serve` takes.
const serveUsage = `usage: nib serve [--addr host:port] [--root dir] [--debug]
  Serves this project's nib as an A2A agent on this machine. Requests
  carry the bearer token kept in the file the start line names.`

// serveSettings is what `nib serve` was told and found before starting.
type serveSettings struct {
	addr, url, root  string
	tokenPath, token string
}

// parseServe reads the flags and checks what needs no startup: the
// address, the root and the token.
func parseServe(args []string) (serveSettings, error) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", defaultServeAddr, "Address to listen on; must be on this machine")
	rootFlag := flags.String("root", "", "Working directory (default: cwd)")
	debugLog := flags.Bool("debug", false, debugUsage)
	flags.Usage = func() { fmt.Fprintln(os.Stderr, serveUsage); flags.PrintDefaults() }
	if err := flags.Parse(args); err != nil {
		return serveSettings{}, setupErr("%v", err)
	}
	setupLogging(*debugLog)
	s := serveSettings{addr: *addr}
	var err error
	if s.url, err = a2a.ServeURL(*addr); err != nil {
		return s, setupErr("%v", err)
	}
	if s.root, err = resolveRoot(*rootFlag); err != nil {
		return s, setupErr("resolve root: %v", err)
	}
	if s.tokenPath, err = a2a.DefaultTokenPath(); err != nil {
		return s, setupErr("%v", err)
	}
	if s.token, err = a2a.Token(s.tokenPath); err != nil {
		return s, setupErr("%v", err)
	}
	return s, nil
}

// runServe is `nib serve`: the session's agent, served over A2A.
func runServe(args []string) error {
	s, err := parseServe(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mem, err := demarkus.Open(ctx, s.root)
	if err != nil {
		return setupErr("memory: %v", err)
	}
	defer func() {
		if cerr := mem.Close(); cerr != nil {
			slog.Warn("memory close", "err", cerr)
		}
	}()
	resolved, provider, err := newProvider(s.root)
	if err != nil {
		return err
	}
	chosen, err := config{}.choices()
	if err != nil {
		return setupErr("%v", err)
	}
	chosen.providerFor = providerFor(func() *llmconfig.Resolved { return resolved })
	chosen.session = newSessionID(time.Now(), "serve")
	assembly, err := assemble(ctx, s.root, buildSystemPrompt(chosen.session), mem.Store, chosen)
	if err != nil {
		return setupErr("%v", err)
	}
	defer closeAssembly(assembly)
	defer assembly.Hooks.SessionEnd(context.WithoutCancel(ctx), "other")
	assembly.StartSession(ctx, "startup")

	handler, err := a2a.NewServer(a2a.ServerConfig{
		Name:        brand.Name,
		Description: "A general agent working in " + filepath.Base(assembly.Root) + ", with its tools, skills and memory.",
		Version:     binaryVersion(),
		URL:         s.url,
		Token:       s.token,
		Skills:      []a2a.Skill{{ID: "general", Name: "General", Description: "Any task in the project: reading, changing and running what it holds."}},
		Run:         serveRun(assembly, provider),
	})
	if err != nil {
		return setupErr("%v", err)
	}
	return listen(ctx, s, handler)
}

// listen serves handler at s.addr until ctx ends, then stops within
// the grace period.
func listen(ctx context.Context, s serveSettings, handler http.Handler) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return setupErr("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stderr, "nib serves A2A at %s (bearer token in %s)\n", s.url, s.tokenPath)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("serve: stop: %w", err)
	}
	return nil
}

// serveRun answers one A2A message with a fresh agent on the session's
// blocks. Nobody is there to ask, so the gate stays autonomous.
func serveRun(assembly *nib.Assembly, provider llm.Provider) func(context.Context, []llm.Message) (string, error) {
	return func(ctx context.Context, history []llm.Message) (string, error) {
		// A new slice: the caller's history keeps the message as it came.
		msgs := append([]llm.Message{{Role: "system", Content: assembly.SystemPrompt}}, history...)
		last := &msgs[len(msgs)-1]
		prompt, err := assembly.Hooks.Prompt(ctx, last.Content)
		if err != nil {
			return "", err
		}
		last.Content = prompt
		ag, err := assembly.Agent(provider)
		if err != nil {
			return "", fmt.Errorf("agent: %w", err)
		}
		defer ag.Close()
		res, err := ag.Run(ctx, msgs)
		if err == nil {
			err = res.Done.Err
		}
		answer := strings.TrimSpace(res.Done.FinalText)
		if again := assembly.Hooks.Stop(ctx, answer, false); again != "" {
			slog.Info("a Stop hook asked the agent to go on; an A2A task does not", "reason", again)
		}
		if err != nil {
			return "", err
		}
		return answer, nil
	}
}

// binaryVersion is the module version nib was built at, for the card.
func binaryVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
