package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
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

// errLoopExited is surfaced when the run goroutine unwinds without the
// loop returning, so AgentEnd still carries a documented reason.
var errLoopExited = errors.New("agent: run goroutine exited before the loop returned")

// runLoop is the per-run goroutine: it brackets [Agent.loop] with
// AgentStart and the cleanup that emits AgentEnd. The cleanup is the
// only path that resets the run state; deferred so a Goexit runs it too.
func (a *Agent) runLoop(ctx context.Context, oneShot bool, result chan<- runResult) {
	a.send(ctx, event.AgentStart{})
	var end event.AgentEnd
	defer func() {
		// A panic here came from a hook or the provider: it fails the
		// run, not the host. No reason and no panic means a Goexit.
		// Either way cleanupRun settles the transcript.
		if r := recover(); r != nil {
			end.Reason = a.endOn(ctx, recovered("run", r))
		} else if end.Reason == "" {
			end.Reason = a.endOn(ctx, errLoopExited)
		}
		a.cleanupRun(ctx, end, result)
	}()
	end.Reason = a.loop(ctx, &end, oneShot)
}

// loop runs turns until the run ends and returns why: turn, tool batch,
// then steering and follow-up before parking for a reply (a one-shot run
// ends instead). [Agent.turn] records usage and final text on end.
func (a *Agent) loop(ctx context.Context, end *event.AgentEnd, oneShot bool) event.EndReason {
	// turnsSinceInput is what [Options.MaxTurns] caps. A delivered reply
	// resets it, so the cap bounds autonomous turns, not the conversation.
	turnsSinceInput := 0
	for {
		if ctx.Err() != nil {
			return event.EndAborted
		}

		// The cap fires only between complete turns, so the previous
		// turn's tool results are in and the transcript is well-formed.
		if a.maxTurns > 0 && turnsSinceInput >= a.maxTurns {
			a.send(ctx, event.MaxTurnsReached{Turns: turnsSinceInput})
			return a.closeAtCap(ctx, end)
		}

		result, err := a.turn(ctx, end, a.toolDefs)
		if err != nil {
			return a.endOn(ctx, err)
		}
		turnsSinceInput++
		assistant := result.Assistant
		a.appendMessage(assistant)
		a.send(ctx, event.TurnEnd{Message: assistant})

		if result.Truncated {
			retry, err := a.handleTruncation(ctx, assistant)
			if err != nil {
				return a.endOn(ctx, err)
			}
			if !retry {
				return event.EndTruncated
			}
			continue
		}

		if len(assistant.ToolCalls) > 0 {
			batchTerminate, err := a.executeToolCalls(ctx, assistant.ToolCalls)
			if err != nil {
				return a.endOn(ctx, err)
			}
			if batchTerminate {
				return stopped(ctx, event.EndTerminated)
			}
			continue
		}

		// No tool calls — turn ended cleanly. Try steering, then
		// follow-up, then park on the reply channel.
		steering, err := a.callSteering(ctx)
		if err != nil {
			return a.endOn(ctx, fmt.Errorf("SteeringMessages: %w", err))
		}
		if len(steering) > 0 {
			a.appendMessages(steering)
			continue
		}

		followup, err := a.callFollowUp(ctx)
		if err != nil {
			return a.endOn(ctx, fmt.Errorf("FollowUpMessages: %w", err))
		}
		if len(followup) > 0 {
			a.appendMessages(followup)
			continue
		}

		// One-shot: nothing is queued and no reply can arrive, so the
		// run is complete. The deferred AgentEnd is the signal.
		if oneShot {
			return stopped(ctx, event.EndCompleted)
		}

		parked, err := a.callBeforePark(ctx)
		if err != nil {
			return a.endOn(ctx, fmt.Errorf("BeforePark: %w", err))
		}
		a.send(ctx, parked)

		input, ok := a.awaitReply(ctx)
		if !ok {
			return event.EndAborted
		}
		a.appendMessage(llm.Message{Role: "user", Content: input})
		// Fresh user input re-arms the turn cap: MaxTurns bounds
		// autonomous turns, not the run's total. It also starts a new
		// exchange, so the earlier answer is no longer the final text.
		turnsSinceInput = 0
		end.FinalText = ""
	}
}

// turn runs one LLM turn offering tools: context transform, stream, then
// usage and final text recorded on end. The caller appends the assistant
// message; end.Usage.Turns numbers the turn.
func (a *Agent) turn(ctx context.Context, end *event.AgentEnd, tools []llm.ToolDef) (turnResult, error) {
	msgs, err := a.transformContext(ctx)
	if err != nil {
		return turnResult{}, fmt.Errorf("TransformContext: %w", err)
	}
	a.send(ctx, event.TurnStart{})

	usage := &end.Usage
	result, err := a.processTurn(ctx, msgs, tools, usage.Turns+1)
	// Before the error check: a turn that broke off mid-stream still
	// returns the text it had written.
	if text := result.Assistant.Content; text != "" {
		end.FinalText = text
	}
	if err != nil {
		return result, err
	}

	usage.Turns++
	usage.ToolCalls += len(result.Assistant.ToolCalls)
	if u := result.Usage; u != nil {
		usage.PromptTokens += u.PromptTokens
		usage.CompletionTokens += u.CompletionTokens
		usage.CachedTokens += u.CachedTokens
		usage.CacheWriteTokens += u.CacheWriteTokens
	}
	return result, nil
}

// closeAtCap ends a run stopped by the turn cap. When [Hooks.OnMaxTurns]
// supplies messages the model first gets one turn without tools, so the
// run ends on an answer instead of mid-work.
func (a *Agent) closeAtCap(ctx context.Context, end *event.AgentEnd) event.EndReason {
	if a.hooks.OnMaxTurns == nil {
		return stopped(ctx, event.EndMaxTurns)
	}
	msgs, err := a.hooks.OnMaxTurns(ctx)
	if err != nil {
		return a.endOn(ctx, fmt.Errorf("OnMaxTurns: %w", err))
	}
	if len(msgs) == 0 {
		return stopped(ctx, event.EndMaxTurns)
	}
	a.appendMessages(msgs)

	result, err := a.turn(ctx, end, nil)
	if err != nil {
		return a.endOn(ctx, err)
	}
	// No tools were offered. A call the model made up anyway is not run;
	// settle answers it when the run ends.
	a.appendMessage(result.Assistant)
	a.send(ctx, event.TurnEnd{Message: result.Assistant})
	if result.Truncated {
		// The cap leaves no turn to retry in: the hook is told, but its
		// wish to retry cannot be honoured.
		if _, err := a.handleTruncation(ctx, result.Assistant); err != nil {
			return a.endOn(ctx, err)
		}
		return event.EndTruncated
	}
	return stopped(ctx, event.EndMaxTurns)
}

// turnResult bundles the per-turn outcomes [processTurn] computes.
// Splitting truncated from the error channel lets the loop dispatch it
// through its own path — truncation goes to the [Hooks.OnTruncated]
// hook while errors funnel into [Agent.emitError].
type turnResult struct {
	// Assistant is the finalized assistant message for this turn.
	// Always populated on a non-error return — including truncated
	// turns, where the hook needs the assistant message and its
	// (possibly partial) tool calls to build rejection replies. On an
	// error return it holds only the text streamed before the turn broke.
	Assistant llm.Message
	// Truncated mirrors the provider's terminal-event Truncated flag.
	// True means the LLM ran out of output budget mid-generation; the
	// caller invokes [Hooks.OnTruncated] before deciding whether to
	// retry or end the run.
	Truncated bool
	// Usage is the provider-reported usage; nil when it reported none.
	Usage *llm.Usage
}

// stopped returns reason for a run that ended by its own doing, unless
// the run ctx was cancelled first: a cancelled run is aborted, however
// far it got, as a parked one would be.
func stopped(ctx context.Context, reason event.EndReason) event.EndReason {
	if ctx.Err() != nil {
		return event.EndAborted
	}
	return reason
}

// recovered turns a recovered panic into an error and logs its stack:
// recovering keeps the host alive, it must not hide the bug.
func recovered(where string, r any) error {
	slog.Error("agent: recovered panic", "where", where, "panic", r, "stack", string(debug.Stack()))
	return fmt.Errorf("agent: panic in %s: %v", where, r)
}

// endOn ends the run on err. Once the run ctx is cancelled the run is an
// abort and err a consequence of it, so it stays silent. The ctx decides,
// not err: a provider's own timeout also reads as DeadlineExceeded.
func (a *Agent) endOn(ctx context.Context, err error) event.EndReason {
	if ctx.Err() != nil {
		return event.EndAborted
	}
	a.emitError(ctx, err)
	return event.EndError
}

// processTurn drives one Stream → drain cycle and returns the
// finalized assistant message plus the truncation signal the loop
// dispatches on.
//
// Streaming events ([event.MessageStart], [event.MessageUpdate],
// [event.MessageEnd]) are emitted as the stream progresses. A truncated
// terminal event is reported via [turnResult.Truncated] (NOT as an
// error) so the loop can route it to the [Hooks.OnTruncated] hook.
func (a *Agent) processTurn(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, turn int) (turnResult, error) {
	a.send(ctx, event.MessageStart{})

	a.setStreaming(true)
	ch, err := a.provider.Stream(ctx, msgs, tools)
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
	// broken ends the turn on err, keeping the text streamed so far so
	// the run's final text does not lose it.
	broken := func(err error) (turnResult, error) {
		a.setStreaming(false)
		return turnResult{Assistant: llm.Message{Role: "assistant", Content: content.String()}}, err
	}

	for {
		select {
		case <-ctx.Done():
			return broken(ctx.Err())
		case ev, ok := <-ch:
			if !ok {
				if err := ctx.Err(); err != nil {
					return broken(err)
				}
				return broken(errProviderClosedEarly)
			}
			if ev.Err != nil {
				// Terminal provider-side failure (streamed error block,
				// payload-limit breach, mid-stream stall): surface the
				// real cause instead of the opaque errProviderClosedEarly.
				return broken(ev.Err)
			}
			if ev.Token != "" {
				content.WriteString(ev.Token)
				a.send(ctx, event.MessageUpdate{Delta: ev.Token})
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
			a.send(ctx, event.MessageEnd{Message: assistant})
			a.emitTurnUsage(ctx, usage, len(toolCalls), turn)

			return turnResult{Assistant: assistant, Truncated: truncated, Usage: usage}, nil
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
		a.emitError(ctx, errProviderTruncated)
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

// maxParallelTools bounds how many calls of one segment execute at once.
const maxParallelTools = 8

// executeToolCalls runs the batch and appends one tool message per call.
// It reports whether every result asked to terminate; an empty batch never
// does. An error ends the run; [Agent.settle] answers what was left open.
func (a *Agent) executeToolCalls(ctx context.Context, calls []llm.ToolCall) (bool, error) {
	if len(calls) == 0 {
		return false, nil
	}

	a.setPendingToolCalls(calls)
	defer a.clearPendingToolCalls()

	allTerminate := true
	for len(calls) > 0 {
		// Consecutive calls to admitted tools run together; any other call
		// runs alone, which is the sequential behaviour.
		n := 0
		for n < len(calls) && a.parallel[calls[n].Function.Name] {
			n++
		}
		n = max(n, 1)
		terminate, err := a.executeSegment(ctx, calls[:n])
		if err != nil {
			return false, err
		}
		allTerminate = allTerminate && terminate
		calls = calls[n:]
	}
	return allTerminate, nil
}

// executeSegment takes calls through three steps so hooks stay serial and
// in call order: BeforeToolCall for each, the tools (together when there
// are several), then AfterToolCall and the result for each.
func (a *Agent) executeSegment(ctx context.Context, calls []llm.ToolCall) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	results := make([]ToolResult, len(calls))
	run := make([]bool, len(calls))
	for i, call := range calls {
		a.send(ctx, event.ToolStart{CallID: call.ID, Name: call.Function.Name, Args: call.Function.Arguments})
		var err error
		if results[i], run[i], err = a.beforeTool(ctx, call); err != nil {
			return false, err
		}
	}

	// On a cancel only calls[:done] have a result. They are kept: the
	// work was done, and settle answers the calls that never started.
	done, cancelled := a.runTools(ctx, calls, run, results)

	allTerminate := true
	var failed error
	for i, call := range calls[:done] {
		result, terminate, err := a.afterTool(ctx, call, results[i])
		if err != nil {
			// The other calls ran too and keep their results. This one's
			// stays out: the hook that failed may redact or cap it.
			failed = cmp.Or(failed, err)
			continue
		}
		allTerminate = allTerminate && terminate
		a.appendMessage(llm.Message{Role: "tool", ToolCallID: call.ID, Content: result.Content})
		a.send(ctx, event.ToolEnd{CallID: call.ID, Result: result.Content, IsError: result.IsError})
	}
	return allTerminate, cmp.Or(failed, cancelled)
}

// runTools executes the calls marked in run, a single one on the loop
// goroutine, several concurrently (at most maxParallelTools at once). No
// tool starts once ctx is done: it returns how many calls have a result.
func (a *Agent) runTools(ctx context.Context, calls []llm.ToolCall, run []bool, results []ToolResult) (int, error) {
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, maxParallelTools)
	for i, call := range calls {
		if !run[i] {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return i, ctx.Err()
		}
		// Checked again: select picks at random when both were ready.
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if len(calls) == 1 {
			results[i] = a.runTool(ctx, call)
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			results[i] = a.runTool(ctx, call)
		})
	}
	return len(calls), nil
}

// unfinishedToolResult answers a tool call the run ended without recording.
// It does not claim the tool never ran: a hook can fail after it did.
const unfinishedToolResult = "Error: no result was recorded for this tool call: the run ended first. The tool may or may not have run."

// settle answers every tool call the last assistant message left open. A
// run can end mid-batch or on a truncated turn, and a provider rejects a
// transcript whose tool calls lack results, so the run could not resume.
func (a *Agent) settle() {
	a.mu.Lock()
	defer a.mu.Unlock()

	answered := make(map[string]bool)
	for i, m := range slices.Backward(a.messages) {
		switch m.Role {
		case "tool":
			answered[m.ToolCallID] = true
		case "assistant":
			// Results go right after the ones already there, before
			// anything a hook appended later.
			at := i + 1
			for at < len(a.messages) && a.messages[at].Role == "tool" {
				at++
			}
			var open []llm.Message
			for _, call := range m.ToolCalls {
				if !answered[call.ID] {
					open = append(open, llm.Message{Role: "tool", ToolCallID: call.ID, Content: unfinishedToolResult})
				}
			}
			a.messages = slices.Insert(a.messages, at, open...)
			return
		}
	}
}

// beforeTool runs BeforeToolCall for call. When the hook blocks the call
// it returns the blocked result and run=false.
func (a *Agent) beforeTool(ctx context.Context, call llm.ToolCall) (blocked ToolResult, run bool, err error) {
	if a.hooks.BeforeToolCall == nil {
		return ToolResult{}, true, nil
	}
	res, err := a.hooks.BeforeToolCall(ctx, BeforeToolCallInput{
		CallID: call.ID,
		Name:   call.Function.Name,
		Args:   call.Function.Arguments,
	})
	if err != nil {
		return ToolResult{}, false, fmt.Errorf("BeforeToolCall(%s): %w", call.Function.Name, err)
	}
	if !res.Block {
		return ToolResult{}, true, nil
	}
	reason := res.Reason
	if reason == "" {
		reason = fmt.Sprintf("tool %q blocked", call.Function.Name)
	}
	return ToolResult{Content: reason, IsError: true}, false, nil
}

// runTool executes call's tool, or answers for one the model made up. A
// panic becomes an error result the model can react to, so one faulty
// tool does not end the run. Safe to call from several goroutines.
func (a *Agent) runTool(ctx context.Context, call llm.ToolCall) (result ToolResult) {
	name := call.Function.Name
	tool, ok := a.tools[name]
	if !ok {
		return toolError(fmt.Errorf("unknown tool %q", name))
	}
	defer func() {
		if r := recover(); r != nil {
			result = toolError(recovered("tool "+name, r))
		}
	}()
	return tool.Execute(ctx, call)
}

// afterTool runs AfterToolCall on a result, blocked ones included, and
// folds in its overrides. The bool is the call's terminate request.
func (a *Agent) afterTool(ctx context.Context, call llm.ToolCall, result ToolResult) (ToolResult, bool, error) {
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

// cleanupRun emits [event.AgentEnd], resets the run state and releases
// [Agent.WaitForIdle]; every way a run can end funnels through it. AgentEnd
// goes out while the run still counts as active: wait for idle before Prompt.
func (a *Agent) cleanupRun(ctx context.Context, end event.AgentEnd, result chan<- runResult) {
	a.settle()
	end.Messages = a.snapshotMessages()
	a.send(ctx, end)

	a.mu.Lock()
	// The error [Agent.Run] returns: what failed the run, or the ctx
	// error when it was aborted (no Error is emitted for an abort).
	err := a.lastErr
	if end.Reason == event.EndAborted {
		err = ctx.Err()
	}
	// A run that ends on its own must still release its ctx, or it stays
	// registered with the caller's ctx. Under the lock so the ctx is done
	// before the agent reads as idle; cancel neither blocks nor calls out.
	if a.cancel != nil {
		a.cancel()
	}
	doneCh := a.doneCh
	a.running = false
	a.streaming = false
	a.cancel = nil
	a.doneCh = nil
	a.inputCh = nil
	a.pendingToolCalls = nil
	a.mu.Unlock()

	// After the reset, so a caller holding the result can start a new run.
	if result != nil {
		result <- runResult{end: end, err: err}
	}
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

// emitError records err (visible via State, returned by Run) and emits
// an [event.Error]. Terminal at this layer: the loop unwinds after it.
func (a *Agent) emitError(ctx context.Context, err error) {
	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
	a.send(ctx, event.Error{Err: err.Error()})
}

// emitTurnUsage forwards the provider-reported usage (when available)
// through [event.TurnUsage]. The foundation reports only what the
// provider returned plus ToolCalls; client-side token estimation is
// application-dependent and belongs in a provider wrapper.
func (a *Agent) emitTurnUsage(ctx context.Context, usage *llm.Usage, toolCalls, turn int) {
	tu := event.TurnUsage{Turn: turn, ToolCalls: toolCalls}
	if usage != nil {
		tu.PromptTokens = usage.PromptTokens
		tu.CompletionTokens = usage.CompletionTokens
		tu.CachedTokens = usage.CachedTokens
	}
	a.send(ctx, tu)
}

// endGrace is how long a closed agent still waits to deliver AgentEnd.
const endGrace = 5 * time.Second

// send delivers ev to the consumer. A control event waits for it or for
// the run ctx: dropping one on a timer would corrupt the stream for a
// consumer that is only slow. Past the ctx it goes out only if there is room.
func (a *Agent) send(ctx context.Context, ev event.Event) {
	if a.events == nil {
		return
	}
	switch ev.(type) {
	case event.MessageUpdate, event.TurnUsage:
		a.offer(ev)
	case event.AgentEnd:
		// Consumers end their read loop on this, so it is never dropped
		// for a slow one. The run ctx is done by now and cannot bound the
		// wait: only [Agent.Close] gives up on a consumer that is gone.
		select {
		case a.events <- ev:
		case <-a.closing:
			// A consumer that is still draining gets its end event.
			timer := time.NewTimer(endGrace)
			defer timer.Stop()
			select {
			case a.events <- ev:
			case <-timer.C:
				slog.Error("agent: AgentEnd not delivered, consumer is gone")
			}
		}
	default:
		select {
		case a.events <- ev:
		case <-ctx.Done():
			a.offer(ev)
		}
	}
}

// offer delivers ev if the channel has room and drops it otherwise.
func (a *Agent) offer(ev event.Event) {
	select {
	case a.events <- ev:
	default:
		slog.Warn("agent: dropping event, channel full", "type", fmt.Sprintf("%T", ev))
	}
}
