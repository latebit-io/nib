package agent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/coding/approval"
	"github.com/latebit-io/junto/coding/budget"
	"github.com/latebit-io/junto/coding/streaming"
	"github.com/latebit-io/junto/engine/event"
)

// Run-goroutine driver for Agent.
//
// run / resumeRun own the goroutine lifecycle: set running=true,
// build (or carry forward) the message slice, drive the inner
// runLoop, then on return commit the savedMessages/savedMode
// snapshot for Resume and emit AgentDone. The defer is gated on a
// runID staleness check so a goroutine whose run has been replaced
// by RunWithMode/Reply does NOT clobber the new run's state.
//
// runLoop is the shared turn-by-turn driver — call processLLMTurn,
// record usage, fire the per-task budget abort if needed, run the
// post-turn nudges, then park on coord.AwaitInput for the next
// developer message. Both run and resumeRun call into it.

// run starts the conversation goroutine for a fresh RunWithMode call.
// Builds the initial message slice from the prompt template and drives
// the runLoop. The deferred staleness-gate commit pattern keeps a
// goroutine whose runID has been bumped from corrupting the live run's
// savedMessages/savedMode snapshot.
func (a *Agent) run(ctx context.Context, runID uint64, coord *approval.Coordinator, fileName, fileContent, goal string, contextFiles []string, mode Mode) {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	success := true // false only on actual errors, not user-initiated cancel
	var messages []llm.Message
	defer func() {
		// Gate ALL mutations on the staleness check. A stale goroutine
		// is unwinding while a replacement run owns the agent's
		// lifecycle; clobbering running/waiting/savedMessages/savedMode
		// here would stomp on the new run's intent. RunWithMode has
		// already cleared savedMessages and the new goroutine will
		// (asynchronously) set running=true — touching that state from
		// the stale goroutine briefly corrupts what Reply()/IsRunning()
		// observe and can leak the old conversation into the new
		// resume snapshot.
		a.mu.Lock()
		stale := runID != a.runID
		if !stale {
			a.waiting = false
			a.running = false
			// Preserve conversation for Resume — cleared by RunWithMode on new session.
			a.savedMessages = messages
			a.savedMode = mode
		}
		a.mu.Unlock()
		if !stale {
			a.send(event.AgentDone{Success: success})
		}
	}()

	if goal == "" {
		goal = "Review this code and suggest improvements, one step at a time."
	}

	memorySummary := a.fetchMemorySummary(ctx)
	messages = a.buildMessages(fileName, fileContent, goal, contextFiles, memorySummary, mode)
	if mode == ModePlanning {
		a.send(event.AgentToken{Text: "Planning...\n\n"})
		a.send(event.AgentStatus{Status: event.StatusPlanning})
	} else {
		a.send(event.AgentToken{Text: "Thinking...\n\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
	}

	thinkState := false
	a.runLoop(ctx, runID, coord, &messages, &success, mode, &thinkState)
}

// resumeRun is the entry point for Resume — starts the run loop with
// pre-existing messages instead of building them from scratch.
func (a *Agent) resumeRun(ctx context.Context, runID uint64, coord *approval.Coordinator, initial []llm.Message, mode Mode) {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	success := true
	messages := initial
	defer func() {
		// Same staleness gate as run() — see that defer's comment.
		a.mu.Lock()
		stale := runID != a.runID
		if !stale {
			a.waiting = false
			a.running = false
			a.savedMessages = messages
			a.savedMode = mode
		}
		a.mu.Unlock()
		if !stale {
			a.send(event.AgentDone{Success: success})
		}
	}()

	a.send(event.AgentToken{Text: "Resuming...\n\n"})
	a.send(event.AgentStatus{Status: event.StatusThinking})

	thinkState := false
	a.runLoop(ctx, runID, coord, &messages, &success, mode, &thinkState)
}

// runLoop is the shared agent loop used by both run and resumeRun.
// The coord parameter is the run's [*approval.Coordinator], captured
// at run-launch time so each goroutine awaits on the channels of its
// own run rather than reading the racy [Agent.coord] field.
func (a *Agent) runLoop(ctx context.Context, runID uint64, coord *approval.Coordinator, messages *[]llm.Message, success *bool, mode Mode, thinkState *bool) {
	activeDefs := a.toolDefs
	if mode == ModePlanning {
		activeDefs = a.planningToolDefs()
	}

	// narrativeNudgeFired guards the post-turn outstanding-work check from
	// firing more than once per developer input. Reset each time a new
	// developer message arrives via inputCh.
	narrativeNudgeFired := false
	// permissionNudgeFired guards the autonomous-mode permission-question
	// gate from firing more than once per developer input. Same lifecycle
	// as narrativeNudgeFired — reset on each inputCh receive.
	permissionNudgeFired := false

	for {
		// Compact old tool results if history is large enough.
		*messages = streaming.MaybeCompact(*messages, activeDefs, a.send)

		var tu budget.Turn
		var err error
		*messages, tu, err = a.processLLMTurn(ctx, runID, *messages, thinkState, activeDefs)

		// Stale-run short-circuit: a competing RunWithMode/Reply
		// replaced this run mid-turn. Skip recordTurnUsage (the runID
		// guard would drop it anyway), abortIfBudgetExceeded, and the
		// AgentWaiting emission — none of those are correct for a run
		// that no longer exists from the developer's perspective. The
		// deferred sender in run() also suppresses AgentDone for stale
		// runs, so the goroutine exits silently and the new run owns
		// the lifecycle.
		if errors.Is(err, errStaleRun) {
			return
		}

		// Record usage regardless of error — partial data is still valuable.
		// Pass runID so late updates from canceled runs are ignored.
		a.recordTurnUsage(runID, tu)

		// Per-task token budget. Enforced after recordTurnUsage so the
		// turn that crosses the threshold has its consumption logged
		// before the run aborts.
		if a.abortIfBudgetExceeded(runID, success) {
			return
		}

		if ctx.Err() != nil {
			*success = false
			return
		}
		// Non-fatal LLM error: stay in the loop and enter waiting state
		// so the developer can adjust and retry. The error was already
		// reported via AgentError inside processLLMTurn.
		if err != nil {
			slog.Debug("LLM turn error, entering wait state for retry", "err", err)
		}

		// Post-turn nudges (narrative, permission). Each is one-shot per
		// developer input. tryInjectPostTurnNudge returns true when one
		// fired so the loop should re-enter processLLMTurn instead of
		// yielding to AgentWaiting. Skipped on error turns because there
		// is no clean assistant content to scan.
		if err == nil && a.tryInjectPostTurnNudge(messages, &narrativeNudgeFired, &permissionNudgeFired) {
			continue
		}

		// Agent's turn is done — wait for the developer's next message.
		// AgentWaiting is critical: if the frontend never sees it, the
		// agent blocks on inputCh with no way for the user to reply.
		a.mu.Lock()
		a.waiting = true
		a.mu.Unlock()
		// Finished requires both an empty task tree AND a clean turn —
		// surfacing DONE on an errored turn with an incidentally-empty
		// tree would mislead the developer into thinking the run
		// completed when it actually bailed out and is awaiting retry.
		finished := err == nil && a.tasksAllComplete()
		if err := a.sendCritical(ctx, event.AgentWaiting{Finished: finished}); err != nil {
			slog.Error("agent waiting delivery failed", "err", err)
			*success = false
			return
		}

		input, err := coord.AwaitInput(ctx)
		if err != nil {
			*success = false
			return
		}
		a.mu.Lock()
		a.waiting = false
		a.intent = input
		a.mu.Unlock()
		narrativeNudgeFired = false
		permissionNudgeFired = false

		// Refresh the system prompt so runtime changes (e.g. coding
		// style switched via SetCodingStyle) take effect immediately.
		(*messages)[0].Content = a.rebuildSystemPrompt(mode)

		*messages = append(*messages, llm.Message{
			Role:    "user",
			Content: input,
		})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		a.send(event.AgentToken{Text: "\n\n"})
	}
}
