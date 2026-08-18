package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// errProviderClosedEarly is returned by processTurn when the provider
// closes its stream channel without first emitting a terminal Done
// event AND ctx is still live. Distinguishes a clean ctx cancellation
// from a provider-side failure (network drop, malformed SSE) so the
// loop can surface the latter as an error instead of treating it as a
// silent partial completion.
var errProviderClosedEarly = errors.New("agent: provider closed stream before completion")

// errProviderTruncated is the sentinel surfaced when the provider's
// terminal Done reports Truncated=true and either no [Hooks.OnTruncated]
// is registered or the registered hook returns Retry=false without
// emitting its own error event. Applications that want recovery wire
// the hook; without it the foundation defaults to ending the run.
var errProviderTruncated = errors.New("agent: provider truncated response")

// runLoop is the per-run goroutine driver. Owns the multi-turn cycle:
// invoke TransformContext → Stream → drain → emit lifecycle events →
// dispatch tools → repeat. Between turns with no tool calls it consults
// SteeringMessages and FollowUpMessages before parking on the
// reply channel; ctx cancellation unwinds the loop.
//
// The deferred cleanup is the only path that resets the run-state
// fields and closes the done channel — every early return funnels
// through it.
func (a *Agent) runLoop(ctx context.Context) {
	a.send(event.AgentStart{})
	defer a.cleanupRun()

	// turn is the 1-indexed turn number within this run, incremented
	// before each LLM call and threaded into the per-turn usage event.
	// turnsSinceInput counts turns taken since the last user message —
	// the quantity [Options.MaxTurns] caps. It tracks turn except that a
	// delivered reply resets it, so the cap bounds autonomous tool-call
	// ping-pong rather than the conversation's total length.
	turn := 0
	turnsSinceInput := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}

		// The cap check sits at the loop top so it fires only between
		// complete turns: the previous turn's tool results are already
		// appended and the transcript is well-formed. Emitting the typed
		// event (not event.Error) keeps limit-stop distinguishable from
		// failure; cleanupRun's AgentEnd follows via the deferred path.
		if a.maxTurns > 0 && turnsSinceInput >= a.maxTurns {
			a.send(event.MaxTurnsReached{Turns: turnsSinceInput})
			return
		}

		msgs, err := a.transformContext(ctx)
		if err != nil {
			a.emitError(fmt.Errorf("TransformContext: %w", err))
			return
		}

		turn++
		turnsSinceInput++
		a.send(event.TurnStart{})

		result, err := a.processTurn(ctx, msgs, turn)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			a.emitError(err)
			return
		}

		assistant := result.Assistant
		a.appendMessage(assistant)
		a.send(event.TurnEnd{Message: assistant})

		if result.Truncated {
			retry, err := a.handleTruncation(ctx, assistant)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				a.emitError(err)
				return
			}
			if !retry {
				return
			}
			continue
		}

		if len(assistant.ToolCalls) > 0 {
			batchTerminate, err := a.executeToolCalls(ctx, assistant.ToolCalls)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				a.emitError(err)
				return
			}
			if batchTerminate {
				return
			}
			continue
		}

		// No tool calls — turn ended cleanly. Try steering, then
		// follow-up, then park on the reply channel.
		steering, err := a.callSteering(ctx)
		if err != nil {
			a.emitError(fmt.Errorf("SteeringMessages: %w", err))
			return
		}
		if len(steering) > 0 {
			a.appendMessages(steering)
			continue
		}

		followup, err := a.callFollowUp(ctx)
		if err != nil {
			a.emitError(fmt.Errorf("FollowUpMessages: %w", err))
			return
		}
		if len(followup) > 0 {
			a.appendMessages(followup)
			continue
		}

		parked, err := a.callBeforePark(ctx)
		if err != nil {
			a.emitError(fmt.Errorf("BeforePark: %w", err))
			return
		}
		a.send(parked)

		input, ok := a.awaitReply(ctx)
		if !ok {
			return
		}
		a.appendMessage(llm.Message{Role: "user", Content: input})
		// Fresh user input re-arms the turn cap: MaxTurns bounds
		// autonomous turns, not the run's total.
		turnsSinceInput = 0
	}
}

// turnResult bundles the per-turn outcomes [processTurn] computes.
// Splitting truncated from the error channel lets the loop dispatch it
// through its own path — truncation goes to the [Hooks.OnTruncated]
// hook while errors funnel into [Agent.emitError].
type turnResult struct {
	// Assistant is the finalized assistant message for this turn.
	// Always populated on a non-error return — including truncated
	// turns, where the hook needs the assistant message and its
	// (possibly partial) tool calls to build rejection replies.
	Assistant llm.Message
	// Truncated mirrors the provider's terminal-event Truncated flag.
	// True means the LLM ran out of output budget mid-generation; the
	// caller invokes [Hooks.OnTruncated] before deciding whether to
	// retry or end the run.
	Truncated bool
}

// processTurn drives one Stream → drain cycle and returns the
// finalized assistant message plus the truncation signal the loop
// dispatches on.
//
// Streaming events ([event.MessageStart], [event.MessageUpdate],
// [event.MessageEnd]) are emitted as the stream progresses. A truncated
// terminal event is reported via [turnResult.Truncated] (NOT as an
// error) so the loop can route it to the [Hooks.OnTruncated] hook.
func (a *Agent) processTurn(ctx context.Context, msgs []llm.Message, turn int) (turnResult, error) {
	a.send(event.MessageStart{})

	a.setStreaming(true)
	ch, err := a.provider.Stream(ctx, msgs, a.toolDefs)
	if err != nil {
		a.setStreaming(false)
		return turnResult{}, fmt.Errorf("provider.Stream: %w", err)
	}

	var (
		content   strings.Builder
		toolCalls []llm.ToolCall
		usage     *llm.Usage
		truncated bool
	)

	for {
		select {
		case <-ctx.Done():
			a.setStreaming(false)
			return turnResult{}, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				a.setStreaming(false)
				if err := ctx.Err(); err != nil {
					return turnResult{}, err
				}
				return turnResult{}, errProviderClosedEarly
			}
			if ev.Err != nil {
				// Terminal provider-side failure (streamed error block,
				// payload-limit breach, mid-stream stall): surface the
				// real cause instead of the opaque errProviderClosedEarly.
				a.setStreaming(false)
				return turnResult{}, ev.Err
			}
			if ev.Token != "" {
				content.WriteString(ev.Token)
				a.send(event.MessageUpdate{Delta: ev.Token})
			}
			if !ev.Done {
				continue
			}
			toolCalls = ev.ToolCalls
			usage = ev.Usage
			truncated = ev.Truncated
			a.setStreaming(false)

			assistant := llm.Message{Role: "assistant", Content: content.String()}
			if len(toolCalls) > 0 {
				assistant.ToolCalls = toolCalls
			}
			// Carry the provider's reasoning trace on the message so it
			// round-trips through history (some providers require it
			// replayed on tool-use turns). Pure passthrough — mirrors
			// ToolCalls above.
			assistant.Reasoning = ev.Reasoning
			a.send(event.MessageEnd{Message: assistant})
			a.emitTurnUsage(usage, len(toolCalls), turn)

			return turnResult{Assistant: assistant, Truncated: truncated}, nil
		}
	}
}

// handleTruncation invokes the [Hooks.OnTruncated] hook (when
// configured) on a truncated turn and returns whether the loop should
// retry. Splice messages from the hook are appended before the next
// turn — or before run-end on Retry=false.
//
// When no hook is registered, the foundation emits its own
// [event.Error] for [errProviderTruncated] and returns retry=false. With
// a hook, the hook owns user-facing error emission on Retry=false; the
// foundation ends the run silently. A hook error is treated as terminal
// and surfaced through [Agent.emitError] by the caller.
func (a *Agent) handleTruncation(ctx context.Context, assistant llm.Message) (retry bool, err error) {
	if a.hooks.OnTruncated == nil {
		a.emitError(errProviderTruncated)
		return false, nil
	}
	res, hookErr := a.hooks.OnTruncated(ctx, TruncationInput{
		Assistant: assistant,
		ToolCalls: assistant.ToolCalls,
	})
	if hookErr != nil {
		return false, fmt.Errorf("OnTruncated: %w", hookErr)
	}
	if len(res.Messages) > 0 {
		a.appendMessages(res.Messages)
	}
	return res.Retry, nil
}

// executeToolCalls dispatches every call in the batch through the
// hook surface (BeforeToolCall / AfterToolCall), appends each result
// as a tool-role message, and returns true when every finalized tool
// result in the batch sets [AfterToolCallResult.Terminate] (matching
// the contract spelled out on that field).
//
// A nil/empty calls slice returns (false, nil). An empty batch is not
// the same as "every result terminates" — the contract requires at
// least one terminate=true result, so an empty batch never terminates
// the run.
func (a *Agent) executeToolCalls(ctx context.Context, calls []llm.ToolCall) (bool, error) {
	if len(calls) == 0 {
		return false, nil
	}

	a.setPendingToolCalls(calls)
	defer a.clearPendingToolCalls()

	allTerminate := true
	for i, call := range calls {
		if err := ctx.Err(); err != nil {
			// The assistant message already carries every tool_use block;
			// a cancel here would leave the unfinished calls without
			// matching tool_result blocks, which Anthropic rejects on
			// resume. Synthesize a cancelled result for each so the
			// transcript stays well-formed.
			a.appendUnfinishedResults(calls[i:], "Error: tool call cancelled before execution.")
			return false, err
		}

		a.send(event.ToolStart{
			CallID: call.ID,
			Name:   call.Function.Name,
			Args:   call.Function.Arguments,
		})

		result, terminate, err := a.dispatchTool(ctx, call)
		if err != nil {
			// A hook error ends the run, but the assistant message already
			// carries this and every later tool_use; give each a result so
			// the transcript resumes cleanly. The raw error travels in
			// event.Error only: the transcript may be replayed to the provider.
			a.appendUnfinishedResults(calls[i:], "Error: tool call aborted before execution.")
			return false, err
		}
		if !terminate {
			allTerminate = false
		}

		a.appendMessage(llm.Message{
			Role:       "tool",
			ToolCallID: call.ID,
			Content:    result.Content,
		})

		a.send(event.ToolEnd{
			CallID:  call.ID,
			Result:  result.Content,
			IsError: result.IsError,
		})
	}

	return allTerminate, nil
}

// appendUnfinishedResults appends a synthesized error tool_result carrying
// content for each tool call that never completed, keeping the transcript
// well-formed when the batch is abandoned mid-flight (ctx cancellation,
// hook error). Anthropic rejects an assistant turn whose tool_use blocks
// lack matching tool_result blocks on resume, so every unfinished call
// must still get a result.
func (a *Agent) appendUnfinishedResults(calls []llm.ToolCall, content string) {
	for _, call := range calls {
		a.appendMessage(llm.Message{
			Role:       "tool",
			ToolCallID: call.ID,
			Content:    content,
		})
	}
}

// dispatchTool runs a single tool call through the BeforeToolCall hook
// (which can block execution), the registered Tool.Execute (or a
// synthesized error for unknown names), and the AfterToolCall hook
// (which can override Content / IsError or signal Terminate). Returns
// the finalized result plus the per-call Terminate flag for the batch
// aggregator. Hook errors are propagated up — they end the run, since
// a hook returning an error usually means the application is in an
// inconsistent state.
func (a *Agent) dispatchTool(ctx context.Context, call llm.ToolCall) (ToolResult, bool, error) {
	beforeCtx := BeforeToolCallInput{
		CallID: call.ID,
		Name:   call.Function.Name,
		Args:   call.Function.Arguments,
	}
	if a.hooks.BeforeToolCall != nil {
		res, err := a.hooks.BeforeToolCall(ctx, beforeCtx)
		if err != nil {
			return ToolResult{}, false, fmt.Errorf("BeforeToolCall(%s): %w", call.Function.Name, err)
		}
		if res.Block {
			reason := res.Reason
			if reason == "" {
				reason = fmt.Sprintf("tool %q blocked", call.Function.Name)
			}
			blocked := ToolResult{Content: reason, IsError: true}
			return a.applyAfterToolCall(ctx, call, blocked)
		}
	}

	tool, ok := a.tools[call.Function.Name]
	var result ToolResult
	if !ok {
		result = ToolResult{
			Content: fmt.Sprintf("Error: unknown tool %q", call.Function.Name),
			IsError: true,
		}
	} else {
		result = tool.Execute(ctx, call)
	}

	return a.applyAfterToolCall(ctx, call, result)
}

// applyAfterToolCall invokes the AfterToolCall hook (when configured)
// and folds its overrides into the result. Returned bool is the
// per-call Terminate flag. Extracted so the Block path and the
// normal path share the same after-hook semantics — without this,
// blocked calls would skip overrides that the application may want to
// apply uniformly (e.g. logging).
func (a *Agent) applyAfterToolCall(ctx context.Context, call llm.ToolCall, result ToolResult) (ToolResult, bool, error) {
	if a.hooks.AfterToolCall == nil {
		return result, false, nil
	}
	res, err := a.hooks.AfterToolCall(ctx, AfterToolCallInput{
		CallID: call.ID,
		Name:   call.Function.Name,
		Args:   call.Function.Arguments,
		Result: result,
	})
	if err != nil {
		return ToolResult{}, false, fmt.Errorf("AfterToolCall(%s): %w", call.Function.Name, err)
	}
	if res.Content != nil {
		result.Content = *res.Content
	}
	if res.IsError != nil {
		result.IsError = *res.IsError
	}
	return result, res.Terminate, nil
}

// transformContext snapshots the current message slice, runs it
// through the TransformContext hook (when configured), and returns
// the (possibly rewritten) slice. The snapshot is a copy so the
// hook cannot accidentally mutate the live transcript while the loop
// continues to append to it.
func (a *Agent) transformContext(ctx context.Context) ([]llm.Message, error) {
	a.mu.Lock()
	msgs := slices.Clone(a.messages)
	a.mu.Unlock()

	if a.hooks.TransformContext == nil {
		return msgs, nil
	}
	out, err := a.hooks.TransformContext(ctx, msgs)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return msgs, nil
	}
	return out, nil
}

// callSteering invokes the SteeringMessages hook (when
// configured) and returns its messages. Steering messages are
// injected after the current turn finishes with NO tool calls and
// re-enter the loop without parking on the reply channel.
func (a *Agent) callSteering(ctx context.Context) ([]llm.Message, error) {
	if a.hooks.SteeringMessages == nil {
		return nil, nil
	}
	return a.hooks.SteeringMessages(ctx)
}

// callFollowUp invokes the FollowUpMessages hook (when
// configured) and returns its messages. Follow-up messages are
// consulted only after steering returns nothing — the layered hook
// surface lets the application distinguish "more work for this turn"
// (steering) from "more work for the run as a whole" (follow-up).
func (a *Agent) callFollowUp(ctx context.Context) ([]llm.Message, error) {
	if a.hooks.FollowUpMessages == nil {
		return nil, nil
	}
	return a.hooks.FollowUpMessages(ctx)
}

// callBeforePark invokes the BeforePark hook (when configured) and
// returns its [event.AgentParked] payload. The foundation has no
// opinion on the payload — the hook owns the application-level "all
// work done" signal. Returns the zero value when the hook is unset.
func (a *Agent) callBeforePark(ctx context.Context) (event.AgentParked, error) {
	if a.hooks.BeforePark == nil {
		return event.AgentParked{}, nil
	}
	return a.hooks.BeforePark(ctx)
}

// awaitReply blocks until [Agent.Reply] delivers a message or ctx is
// cancelled. Returns (input, true) on a delivered message,
// ("", false) when ctx is cancelled. The inputCh is allocated per
// run, so a Reply that arrives after the loop exits cannot leak into
// the next run.
func (a *Agent) awaitReply(ctx context.Context) (string, bool) {
	a.mu.Lock()
	ch := a.inputCh
	a.mu.Unlock()
	if ch == nil {
		return "", false
	}
	select {
	case input := <-ch:
		return input, true
	case <-ctx.Done():
		return "", false
	}
}

// cleanupRun emits [event.AgentEnd], resets the run-state fields under
// [Agent.mu], and closes the done channel [Agent.WaitForIdle] parks on.
// Called from runLoop's defer so every return path — clean completion,
// hook error, ctx cancellation — funnels through one place. There is no
// recover: a panic in a hook or tool still crashes the process.
//
// AgentEnd is delivered while running is still true, so a consumer that
// reacts to AgentEnd by calling Prompt must [Agent.WaitForIdle] first or
// it may see [ErrRunInProgress].
func (a *Agent) cleanupRun() {
	a.send(event.AgentEnd{Messages: a.snapshotMessages()})

	a.mu.Lock()
	doneCh := a.doneCh
	a.running = false
	a.streaming = false
	a.cancel = nil
	a.doneCh = nil
	a.inputCh = nil
	a.pendingToolCalls = nil
	a.mu.Unlock()

	if doneCh != nil {
		close(doneCh)
	}
}

// snapshotMessages returns a defensive copy of the current transcript
// for inclusion in [event.AgentEnd]. Frontends inspecting the final
// transcript can mutate the slice without affecting any subsequent
// run's bookkeeping.
func (a *Agent) snapshotMessages() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.messages)
}

// appendMessage appends a single message to the run's transcript
// under [Agent.mu]. The mutex is released before the loop continues
// so [Agent.State] and [Agent.Reply] callers do not block on the
// loop's own work.
func (a *Agent) appendMessage(msg llm.Message) {
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.mu.Unlock()
}

// appendMessages appends a batch of messages to the transcript.
// Equivalent to repeated appendMessage but takes the lock once.
func (a *Agent) appendMessages(msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	a.mu.Lock()
	a.messages = append(a.messages, msgs...)
	a.mu.Unlock()
}

// setStreaming flips the streaming flag visible through [Agent.State].
func (a *Agent) setStreaming(on bool) {
	a.mu.Lock()
	a.streaming = on
	a.mu.Unlock()
}

// setPendingToolCalls records the active batch of tool call IDs so
// [Agent.State] callers can observe in-flight dispatches. Replaces the
// previous map outright — a new batch shadows the previous one.
func (a *Agent) setPendingToolCalls(calls []llm.ToolCall) {
	pending := make(map[string]bool, len(calls))
	for _, c := range calls {
		pending[c.ID] = true
	}
	a.mu.Lock()
	a.pendingToolCalls = pending
	a.mu.Unlock()
}

// clearPendingToolCalls drops the tool-call ID map after the batch
// completes. Pairs with [Agent.setPendingToolCalls] via defer in
// [Agent.executeToolCalls].
func (a *Agent) clearPendingToolCalls() {
	a.mu.Lock()
	a.pendingToolCalls = nil
	a.mu.Unlock()
}

// emitError records err on Agent.lastError (visible via State) and
// emits an [event.Error] for the frontend. The loop unwinds after
// calling this — error events are terminal at the foundation layer;
// applications that want recoverable errors must filter at their own
// boundary.
func (a *Agent) emitError(err error) {
	msg := err.Error()
	a.mu.Lock()
	a.lastError = msg
	a.mu.Unlock()
	a.send(event.Error{Err: msg})
}

// emitTurnUsage forwards the provider-reported usage (when available)
// through [event.TurnUsage]. The foundation reports only what the
// provider returned plus ToolCalls; client-side token estimation is
// application-dependent and belongs in a provider wrapper.
func (a *Agent) emitTurnUsage(usage *llm.Usage, toolCalls, turn int) {
	tu := event.TurnUsage{Turn: turn, ToolCalls: toolCalls}
	if usage != nil {
		tu.PromptTokens = usage.PromptTokens
		tu.CompletionTokens = usage.CompletionTokens
		tu.CachedTokens = usage.CachedTokens
	}
	a.send(tu)
}

// send delivers an event to the frontend. High-volume streaming events
// ([event.MessageUpdate], [event.TurnUsage]) are best-effort: dropped with a warning when the channel is full.
// Control-flow events block up to 5s before being dropped with an error
// log; an undrained channel that long indicates the consumer has
// stalled and the agent cannot make progress regardless.
func (a *Agent) send(ev event.Event) {
	switch ev.(type) {
	case event.MessageUpdate, event.TurnUsage:
		select {
		case a.events <- ev:
		default:
			slog.Warn("agent: dropping streaming event, channel full",
				"type", fmt.Sprintf("%T", ev))
		}
	default:
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case a.events <- ev:
		case <-timer.C:
			slog.Error("agent: failed to deliver event, channel full",
				"type", fmt.Sprintf("%T", ev))
		}
	}
}
