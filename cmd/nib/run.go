package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
// store cannot wedge nib shutdown.
const indexWriteTimeout = 5 * time.Second

// runAgent boots a kit agent on the given prompt, drives the event
// stream through [kit/headless.Runner] in single-shot mode, writes the
// resulting index entry, and prints the agent's final summary to stdout.
//
// The binary owns the index write — not the agent — so the index stays
// consistent even when the agent runs out of budget mid-update.
func runAgent(ctx context.Context, root string, store memory.Store, message string, chosen choices) error {
	_, provider, err := newProvider(root)
	if err != nil {
		return err
	}

	sessionID := newSessionID(time.Now(), message)
	slog.Debug("session", "id", sessionID)

	assembly, err := assemble(ctx, root, buildSystemPrompt(sessionID), store, chosen)
	if err != nil {
		return setupErr("%v", err)
	}
	defer closeAssembly(assembly)
	ag, err := assembly.Agent(provider)
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

	recordSession(store, sessionID, message, status)

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

// recordSession adds the session to the index. A failure does not
// change the run's outcome, but is said aloud: --list would otherwise
// be silently out of date.
func recordSession(store memory.Store, sessionID, message string, status sessionStatus) {
	// A fresh context: the run's may already be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), indexWriteTimeout)
	defer cancel()
	if err := writeIndexEntry(ctx, store, sessionID, message, status); err != nil {
		warn("update "+indexPath, err)
	}
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
