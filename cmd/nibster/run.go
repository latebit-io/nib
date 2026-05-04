package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/tools/bash"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
)

// eventBufferSize is the kit events channel capacity. Generous enough
// that high-volume token streams from a chatty model never wedge the
// translator on a slow consumer.
const eventBufferSize = 128

// indexWriteTimeout bounds the index-update RPC so a stuck demarkus
// store cannot wedge nibster shutdown.
const indexWriteTimeout = 5 * time.Second

// runResult is the outcome of a single agent run, derived from the kit
// event stream. parkedNaturally flips true when the agent completes a
// turn with no tool calls (the foundation's natural "I'm done" signal,
// surfaced via the GetFollowUpMessages hook); errs accumulates anything
// the loop emitted as [event.AgentError] before that point.
type runResult struct {
	summary          string
	errs             []string
	parkedNaturally  bool
	contextCancelled bool
	summaryTruncated bool
}

// runAgent boots a kit agent on the given prompt, drives the event
// stream to completion, writes the resulting index entry, and prints
// the agent's final summary to stdout.
//
// The binary owns the index write — not the agent — so the index stays
// consistent even when the agent runs out of budget mid-update.
//
// nibster does not use [kit/headless.Runner] for the drive loop. The
// runner relies on [event.AgentWaiting], which kit does not emit
// natively (it has no foundation event for "loop is parked"); the
// idiomatic kit-consumer workaround — sending AgentWaiting from a
// GetFollowUpMessages hook — races the kit translator on the consumer
// events channel, so prior MessageUpdate→AgentToken events can arrive
// after AgentWaiting and the runner exits with an empty Summary. We
// drive a small drain loop directly off [event.AgentDone], which kit
// emits in order through the translator pipeline. (See plan
// /nib/plans/nibster.md — this is one of the kit gaps the smoke test
// was designed to surface.)
func runAgent(ctx context.Context, root string, store memory.Store, message string) error {
	_, resolved := llmconfig.Resolve(root)
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

	events := make(chan event.Event, eventBufferSize)

	// parked latches when the agent reaches the GetFollowUpMessages
	// hook — i.e., the foundation has no tool calls left to issue and
	// is about to await user input. We treat that state as success and
	// use it (in driveAgent) to override the AgentDone.Success that
	// kit will emit after we Cancel.
	var parked atomic.Bool

	// agRef holds a pointer to the agent so the hook (built before
	// kit.New returns) can call Cancel on it. Safe because the
	// foundation only fires hooks during a run, which begins after
	// Prompt is called — at which point agRef is non-nil.
	var agRef *kit.Agent

	ag, err := kit.New(kit.Config{
		Provider:     provider,
		Events:       events,
		SystemPrompt: buildSystemPrompt(sessionID),
		Tools: []kit.Tool{
			bash.New(root),
			memorytools.NewFetchTool(store),
			memorytools.NewPublishTool(store),
			memorytools.NewAppendTool(store),
			memorytools.NewListTool(store),
		},
		Hooks: kit.Hooks{
			GetFollowUpMessages: parkAndCancel(&parked, &agRef),
		},
	})
	if err != nil {
		return setupErr("agent: %v", err)
	}
	agRef = ag

	if err := ag.Prompt(ctx, message); err != nil {
		ag.Close()
		return setupErr("prompt: %v", err)
	}

	result := driveAgent(ctx, ag, events, &parked)

	// Close ensures the translator goroutine exits and any final
	// foundation AgentEnd is fully drained. The drain goroutine inside
	// Close handles late events arriving after we returned from the
	// drive loop.
	closeAgent(ag, events)

	status := classifyStatusFromResult(result)

	// Best-effort index write — failures do not mask the agent
	// outcome but DO surface to stderr. Going through slog alone would
	// be invisible in the default path (setupLogging without --debug
	// routes slog to io.Discard), leaving --list and --show silently
	// out of sync with reality. Use a fresh context so a cancelled
	// parent ctx doesn't also kill the index update.
	indexCtx, cancel := context.WithTimeout(context.Background(), indexWriteTimeout)
	defer cancel()
	if ierr := writeIndexEntry(indexCtx, store, sessionID, message, status); ierr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update %s: %v\n", indexPath, ierr)
		slog.Warn("index entry", "err", ierr)
	}

	if result.summary != "" {
		fmt.Println(result.summary)
	}
	if status != statusSuccess {
		if len(result.errs) > 0 {
			return fmt.Errorf("agent completed with errors: %s", result.errs[0])
		}
		if result.contextCancelled {
			return fmt.Errorf("agent run cancelled")
		}
		return fmt.Errorf("agent completed with errors")
	}
	return nil
}

// parkAndCancel returns a [kit.Hooks.GetFollowUpMessages] hook that
// latches the parked flag and cancels the agent. Cancel is the only
// way to make a kit foundation exit a parked run; without it, the
// foundation waits forever on the awaitReply select. We invoke it here
// (on the foundation goroutine) and let the resulting AgentEnd flow
// through the translator pipeline so AgentDone arrives on the consumer
// channel after every prior translated event.
func parkAndCancel(parked *atomic.Bool, agRef **kit.Agent) func(context.Context) ([]llm.Message, error) {
	return func(_ context.Context) ([]llm.Message, error) {
		parked.Store(true)
		if a := *agRef; a != nil {
			a.Cancel()
		}
		return nil, nil
	}
}

// driveAgent drains the event channel until [event.AgentDone] arrives
// or ctx is cancelled. AgentToken events feed the summary; AgentError
// events accumulate. The returned [runResult] tells runAgent whether
// the run completed naturally (parked.Load() == true) or was
// terminated by an error or external cancellation.
func driveAgent(ctx context.Context, ag *kit.Agent, events <-chan event.Event, parked *atomic.Bool) runResult {
	var summary strings.Builder
	var errs []string
	contextCancelled := false
	truncated := false

	for {
		select {
		case <-ctx.Done():
			if !contextCancelled {
				contextCancelled = true
				ag.Cancel()
			}
		case ev, ok := <-events:
			if !ok {
				return runResult{
					summary:          summary.String(),
					errs:             errs,
					parkedNaturally:  parked.Load(),
					contextCancelled: contextCancelled,
					summaryTruncated: truncated,
				}
			}
			switch e := ev.(type) {
			case event.AgentToken:
				appendBounded(&summary, e.Text, &truncated)
			case event.AgentError:
				errs = append(errs, e.Err)
			case event.AgentDone:
				return runResult{
					summary:          summary.String(),
					errs:             errs,
					parkedNaturally:  parked.Load(),
					contextCancelled: contextCancelled,
					summaryTruncated: truncated,
				}
			}
		}
	}
}

// maxSummaryBytes caps the in-memory summary buffer. Tokens beyond
// this still stream out via [event.AgentToken] handlers in other
// consumers, but nibster only retains the first 10 MB to bound a
// runaway transcript.
const maxSummaryBytes = 10 * 1024 * 1024

// appendBounded adds text to summary, capping at maxSummaryBytes with
// a one-time truncation marker. Mirrors the policy in kit/headless so
// nibster's behavior matches what other kit consumers see.
//
// truncated is the explicit latch — once set, further calls are
// no-ops. Without it, a single oversize chunk would write the marker
// while leaving summary.Len() below the cap, and subsequent small
// appends would keep growing the summary after the "truncated" notice
// — breaking the one-time-truncation contract.
func appendBounded(summary *strings.Builder, text string, truncated *bool) {
	if *truncated {
		return
	}
	if summary.Len()+len(text) > maxSummaryBytes {
		summary.WriteString("\n…[summary truncated]")
		*truncated = true
		return
	}
	summary.WriteString(text)
}

// closeAgent shuts down the agent and drains any final events so the
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

// classifyStatusFromResult derives the index entry's status marker
// from the run outcome. parkedNaturally takes precedence — the agent
// produced its final answer and the foundation was about to wait —
// even though kit reports AgentDone.Success=false because we Cancel
// to unwind the park.
func classifyStatusFromResult(r runResult) sessionStatus {
	switch {
	case r.parkedNaturally && len(r.errs) == 0:
		return statusSuccess
	case r.contextCancelled:
		return statusCancelled
	default:
		return statusFailed
	}
}
