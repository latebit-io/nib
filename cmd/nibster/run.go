package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/ai/oauth"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/headless"
	"github.com/latebit-io/nib/kit/memory"
)

// eventBufferSize is the kit events channel capacity. Generous enough
// that high-volume token streams from a chatty model never wedge the
// translator on a slow consumer.
const eventBufferSize = 128

// indexWriteTimeout bounds the index-update RPC so a stuck demarkus
// store cannot wedge nibster shutdown.
const indexWriteTimeout = 5 * time.Second

// runAgent boots a kit agent on the given prompt, drives the event
// stream through [kit/headless.Runner] in single-shot mode, writes the
// resulting index entry, and prints the agent's final summary to stdout.
//
// The binary owns the index write — not the agent — so the index stays
// consistent even when the agent runs out of budget mid-update.
func runAgent(ctx context.Context, root string, store memory.Store, message string) error {
	resolved, err := resolveProvider(root)
	if err != nil {
		return setupErr("oauth store: %v", err)
	}
	if !resolved.HasProvider() {
		return setupErr("no LLM credentials — %s", credentialHint(resolved))
	}
	provider := resolved.NewProvider()
	if provider == nil {
		return setupErr("provider construction failed for profile %q", resolved.Profile)
	}
	slog.Debug("provider resolved", "profile", resolved.Profile, "model", resolved.Model)

	sessionID := newSessionID(time.Now(), message)
	slog.Debug("session", "id", sessionID)

	// Discard the skill.Result diagnostics here — Discover already logs
	// each refusal/shadow via slog; the Result detail is only needed for
	// the --plugins manifest (see printPlugins).
	toolset, _ := nibsterToolset(root, store)
	ag, err := kit.New(kit.Config{
		Provider:     provider,
		SystemPrompt: buildSystemPrompt(sessionID),
		Toolset:      toolset,
	})
	if err != nil {
		return setupErr("agent: %v", err)
	}

	sub, err := ag.Subscribe(kit.SubscribeOptions{BufferSize: eventBufferSize})
	if err != nil {
		return setupErr("subscribe: %v", err)
	}
	events := sub.Events()

	// Single-shot Runner: AgentWaiting (emitted by the foundation just
	// before parking on awaitReply, translated by kit) triggers Cancel +
	// Success=true. AgentToken events accumulate into Result.Summary.
	runner := headless.New(ag, events)
	result := runner.Run(ctx, message)

	// Close drains the translator and any trailing AgentEnd → AgentDone
	// the foundation emits in response to the Runner's Cancel; without a
	// concurrent drain, the translator would block on the still-buffered
	// subscription inbox.
	closeAgent(ag, events)

	status := classifyStatus(ctx, result)

	// Best-effort index write — failures do not mask the agent outcome
	// but DO surface to stderr. Going through slog alone would be
	// invisible in the default path (setupLogging without --debug routes
	// slog to io.Discard), leaving --list and --show silently out of
	// sync with reality. Use a fresh context so a cancelled parent ctx
	// doesn't also kill the index update.
	indexCtx, cancel := context.WithTimeout(context.Background(), indexWriteTimeout)
	defer cancel()
	if ierr := writeIndexEntry(indexCtx, store, sessionID, message, status); ierr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update %s: %v\n", indexPath, ierr)
		slog.Warn("index entry", "err", ierr)
	}

	if result.Summary != "" {
		fmt.Println(result.Summary)
	}
	if status != statusSuccess {
		if len(result.Errors) > 0 {
			return fmt.Errorf("agent completed with errors: %s", result.Errors[0])
		}
		return fmt.Errorf("agent completed with errors")
	}
	return nil
}

// closeAgent shuts down the agent and drains any trailing events so the
// kit translator goroutine can exit cleanly. Close emits a final
// AgentDone (Success=false because Close calls Cancel internally);
// without a consumer it would wedge the translator on a blocking
// control-event send.
func closeAgent(ag *kit.Agent, events <-chan event.Event) {
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
}

// resolveProvider resolves the LLM profile for root and, for OAuth-only
// profiles (chatgpt, copilot), attaches the auth store so HasProvider
// can succeed. Stored-key wiring is nib-code-only (TUI-entered keys), so
// it's intentionally omitted. Gated on OAuthProvider so API-key users
// never touch the auth file — a corrupt store is only reported when
// OAuth is actually the configured auth path. The Resolved is returned
// even on error so callers can still render a credential hint.
func resolveProvider(root string) (*llmconfig.Resolved, error) {
	_, resolved := llmconfig.Resolve(root)
	if resolved.OAuthProvider == "" {
		return resolved, nil
	}
	oauthStore, err := openOAuthStore()
	if err != nil {
		return resolved, err
	}
	llmconfig.WireOAuth(resolved, oauthStore)
	return resolved, nil
}

// openOAuthStore opens the default OAuth token store. A missing
// auth.json is not an error ([oauth.NewStore] treats os.ErrNotExist
// as empty); only real failures (no user config dir, unreadable file,
// corrupt JSON) surface here so the caller can distinguish "user
// hasn't logged in yet" from "store is broken."
func openOAuthStore() (*oauth.Store, error) {
	path, err := oauth.DefaultStorePath()
	if err != nil {
		return nil, fmt.Errorf("resolve store path: %w", err)
	}
	store, err := oauth.NewStore(path)
	if err != nil {
		return nil, fmt.Errorf("open store at %s: %w", path, err)
	}
	return store, nil
}

// classifyStatus derives the index entry's status marker from the
// runner result. The Runner exits cleanly on [event.AgentWaiting]
// (single-shot mode) with Success=true; ctx cancellation surfaces as
// Success=false plus a non-nil ctx.Err on the parent context.
func classifyStatus(ctx context.Context, r *headless.Result) sessionStatus {
	if r.Success {
		return statusSuccess
	}
	if ctx.Err() != nil {
		return statusCancelled
	}
	return statusFailed
}
