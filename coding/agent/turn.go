package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/budget"
	"github.com/latebit-io/nib/coding/streaming"
	"github.com/latebit-io/nib/coding/truncation"
	"github.com/latebit-io/nib/engine/event"
)

// Per-turn pipeline for Agent.
//
// processLLMTurn is the inner loop that drives a single round-trip
// (or multi-iteration tool dance) with the LLM provider.
// executeToolCalls dispatches the tool calls returned by the
// stream, and dispatchTool runs each one through the planning
// blocklist and active-task gate before calling its Execute method.
// flushDirtyBuffers coordinates with the frontend to autosave open
// buffers before each dispatch, and afterToolDispatch handles
// per-tool cleanup (the bash tool invalidates the cache because it
// can mutate disk outside the edit-approval flow).
//
// Streaming + compaction primitives live in [coding/streaming],
// truncation recovery in [coding/truncation], the run-loop driver
// (run / resumeRun / runLoop) in run.go.

// planningToolDefs returns the tool definitions with write-side tools removed.
func (a *Agent) planningToolDefs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(a.toolDefs))
	for _, def := range a.toolDefs {
		name := strings.ToLower(def.Function.Name)
		if a.planningBlocklist[name] {
			continue
		}
		defs = append(defs, def)
	}
	return defs
}

// afterToolDispatch performs post-dispatch cleanup for tools that modify
// the filesystem outside the edit approval flow (e.g. bash). Invalidates
// the file cache and asks the frontend to reload open buffers.
func (a *Agent) afterToolDispatch(toolName string) {
	if toolName == "bash" {
		a.cache.Reset("", "")
		a.send(event.ReloadBuffers{})
	}
}

// processLLMTurn runs the LLM loop for one agent turn: stream responses,
// dispatch tool calls, repeat until no tool calls remain. Returns the
// updated messages list, accumulated usage, or an error if the turn could
// not complete. toolDefs controls which tools the LLM can invoke for this
// turn. runID is the generation token of the calling run — checked at
// each inner iteration so a new RunWithMode/Reply that increments runID
// while this turn is mid-flight short-circuits before the next Stream
// call instead of spending tokens on a request whose accounting will be
// dropped by [Agent.recordTurnUsage]'s runID guard.
func (a *Agent) processLLMTurn(ctx context.Context, runID uint64, messages []llm.Message, thinkState *bool, toolDefs []llm.ToolDef) ([]llm.Message, budget.Turn, error) {
	var tu budget.Turn
	truncationRetries := 0
	for {
		// Stale-run guard. RunWithMode/Reply increments a.runID, resets
		// sessionUsage, releases mu, then calls prevCancel(). In the
		// window between the unlock and prevCancel, the old goroutine
		// could pass shouldAbortForBudget (sessionUsage just reset),
		// reach Stream, and burn tokens on a Stream call whose
		// accounting recordTurnUsage will drop. Catching the staleness
		// here narrows the window to "between this unlock and the
		// Stream call below" (~tens of ns) — not zero, but practically
		// closed. Returning [errStaleRun] (NOT context.Canceled) lets
		// the run loop short-circuit before any post-turn UI events
		// fire; if we returned ctx.Canceled, the run loop's `ctx.Err()
		// != nil` check would still see ctx as live (prevCancel has
		// not yet fired) and the stale goroutine would emit
		// AgentWaiting for a run that has already been replaced.
		a.mu.Lock()
		stale := runID != a.runID
		a.mu.Unlock()
		if stale {
			return messages, tu, errStaleRun
		}

		// Per-task budget check BEFORE the next provider Stream. A
		// single agent turn can call Stream many times (one per tool-
		// call round-trip), and the post-turn check in runLoop only
		// fires after this whole function returns. Without this gate, a
		// runaway tool-call loop could spend 10× the budget before the
		// outer loop notices. Returning early hands tu back so
		// recordTurnUsage in runLoop commits the partial accounting,
		// after which abortIfBudgetExceeded fires the real abort path.
		// The transcript is well-formed at this point — the prior
		// iteration's executeToolCalls appended every required tool
		// reply before looping back here.
		if a.shouldAbortForBudget(tu) {
			slog.Warn("agent: per-task budget would be exceeded by next Stream; aborting turn early",
				"prompt_tokens_pending", tu.PromptTokens,
				"completion_tokens_pending", tu.CompletionTokens)
			return messages, tu, nil
		}

		// Inject pending lint violations as a user message so the LLM
		// treats them as a high-priority instruction. Checked each iteration
		// because waitForContinue (called during dispatchTool) may set
		// pendingLint mid-loop after an edit is approved.
		if msg := a.drainPendingLint(); msg != "" {
			messages = append(messages, llm.Message{Role: "user", Content: msg})
		}

		tu.LastEstimate = streaming.EstimateAndBroadcast(messages, toolDefs, a.send)

		ch, err := a.currentProvider().Stream(ctx, messages, toolDefs)
		if err != nil {
			slog.Error("stream failed", "err", err)
			a.send(event.AgentError{Err: fmt.Sprintf("LLM error: %v", err)})
			return messages, tu, err
		}

		result, err := streaming.Drain(ctx, ch, thinkState, a.send)
		tu.AddUsage(result.Usage)
		tu.CompletionEst += llm.EstimateTokens(result.Content)
		if err != nil {
			slog.Error("agent: stream closed before completion", "err", err)
			a.send(event.AgentError{Err: fmt.Sprintf("LLM stream error: %v", err)})
			return messages, tu, err
		}
		toolCalls, truncated := result.ToolCalls, result.Truncated

		if ctx.Err() != nil {
			return messages, tu, ctx.Err()
		}

		assistantMsg := llm.Message{
			Role:    "assistant",
			Content: result.Content,
		}
		if len(toolCalls) > 0 {
			assistantMsg.ToolCalls = toolCalls
		}
		messages = append(messages, assistantMsg)

		if truncated {
			var retryErr error
			messages, truncationRetries, retryErr = truncation.Recover(messages, toolCalls, truncationRetries, a.currentProvider(), a.send)
			if retryErr != nil {
				return messages, tu, retryErr
			}
			continue
		}
		truncationRetries = 0

		// No tool calls — agent's turn is done.
		if len(toolCalls) == 0 {
			return messages, tu, nil
		}
		var toolErr error
		messages, toolErr = a.executeToolCalls(ctx, messages, toolCalls, &tu)
		if toolErr != nil {
			return messages, tu, toolErr
		}
	}
}

// executeToolCalls dispatches each tool call from one LLM turn, flushing
// dirty buffers beforehand and appending the tool result as a tool-role
// message. Lint-pending calls are skipped with a placeholder reply so the
// LLM sees the fix-lint-first directive without losing the tool-call ID
// linkage. Returns an error when autosave fails or ctx is cancelled mid-
// dispatch so the caller can end the turn visibly.
//
// In non-autonomous, non-headless (interactive) mode, only the FIRST
// file-edit tool in the batch is dispatched; subsequent edit_file /
// write_file / replace_file calls are rejected with a tool-result error
// that steers the model back to one-edit-per-turn. This replaces the
// prompt rule "One file-edit tool call per interactive turn" with
// deterministic enforcement so the model can't drift past it.
func (a *Agent) executeToolCalls(ctx context.Context, messages []llm.Message, toolCalls []llm.ToolCall, tu *budget.Turn) ([]llm.Message, error) {
	enforceSingleEdit := !a.currentAutonomous() && a.interactionMode != Headless
	editFired := false

	for _, tc := range toolCalls {
		if a.hasLintPending() {
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    "Skipped — fix style lint violations first.",
			})
			continue
		}

		toolName := strings.ToLower(tc.Function.Name)
		if enforceSingleEdit && fileEditTools[toolName] {
			if editFired {
				slog.Info("agent: rejecting extra file-edit in same turn",
					"tool", toolName, "id", tc.ID)
				messages = append(messages, llm.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content: "Skipped — only ONE file-edit per turn in interactive mode. " +
						"Wait for the developer to review the previous edit, then make this change " +
						"in a follow-up turn. The next tool result will include the updated file content.",
				})
				continue
			}
			editFired = true
		}

		if err := a.flushDirtyBuffers(ctx); err != nil {
			a.send(event.AgentError{Err: fmt.Sprintf("autosave failed: %v", err)})
			return messages, err
		}
		// Count only tool calls that pass the autosave gate. An
		// autosave failure short-circuits before any tool dispatch
		// happens, so counting at the top of the loop would inflate
		// AgentTurnUsage.ToolCalls for tools that never actually ran.
		tu.ToolCalls++
		slog.Debug("tool call", "name", tc.Function.Name, "id", tc.ID)
		a.send(event.AgentToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})

		result := a.dispatchTool(ctx, tc)
		if ctx.Err() != nil {
			return messages, ctx.Err()
		}

		a.afterToolDispatch(tc.Function.Name)
		messages = append(messages, llm.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Content:    result,
		})
	}
	return messages, nil
}

// flushDirtyBuffers asks the frontend to save all dirty buffers to disk,
// then invalidates the corresponding cache entries. The actual I/O runs
// on the frontend's goroutine (via FlushBuffers event) so we never touch
// TUI-owned buffer state from the agent goroutine. Called before each
// tool dispatch — not once per batch — because an earlier tool (e.g.
// edit_file) may modify buffers that a later tool needs on disk.
func (a *Agent) flushDirtyBuffers(ctx context.Context) error {
	resultCh := make(chan event.FlushResult, 1)

	// Enqueue with a bounded timeout — if the frontend isn't draining
	// events, fail visibly rather than blocking the agent indefinitely.
	enqueueTimeout := time.NewTimer(5 * time.Second)
	defer enqueueTimeout.Stop()
	select {
	case a.events <- event.FlushBuffers{Result: resultCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-enqueueTimeout.C:
		return fmt.Errorf("autosave: event queue not draining")
	}

	// Wait for the frontend to complete the save.
	responseTimeout := time.NewTimer(5 * time.Second)
	defer responseTimeout.Stop()
	select {
	case res := <-resultCh:
		for _, p := range res.Saved {
			a.cache.Invalidate(p)
		}
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	case <-responseTimeout.C:
		return fmt.Errorf("autosave: frontend response timed out")
	}
}

// dispatchTool executes a tool call and returns the body fed back to
// the LLM. Side effects (navigation, file-created notifications, edit
// approval, task review) are owned by the tools themselves via
// collaborator interfaces — the dispatcher just enforces the planning
// blocklist and the active-task gate, then calls Execute.
func (a *Agent) dispatchTool(ctx context.Context, tc llm.ToolCall) string {
	name := strings.ToLower(tc.Function.Name)

	// Enforce planning mode blocklist at dispatch time — the schema filter
	// removes tools from the advertised list, but a model could still emit
	// a blocked tool call. Reject it before execution.
	if a.currentMode() == ModePlanning && a.planningBlocklist[name] {
		return fmt.Sprintf("Error: tool %q is not available in planning mode", tc.Function.Name)
	}

	// Execution gate: mutating tools require an active [>] task in
	// /project.md so every code change is tracked in the work tree.
	if msg := a.enforceActiveTaskGate(ctx, name); msg != "" {
		return msg
	}

	tool, ok := a.tools[name]
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Function.Name) + a.intentReminder()
	}

	result := tool.Execute(ctx, tc)
	return result.Content + a.intentReminder()
}
