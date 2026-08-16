// Package tool hosts bundled [github.com/latebit-io/nib/kit.ToolDecorator]
// implementations. Import as `tooldec "github.com/latebit-io/nib/kit/decorate/tool"`
// to wrap a [kit.Tool] with tracing, rate-limiting, dry-run, sandboxing, etc.
package tool

import (
	"context"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/dyncontext"
)

// TraceEvent is one observation emitted by [WithTracing]'s emit
// callback. Carries the tool name plus the call's request and result
// payloads so observers (loggers, OpenTelemetry exporters, test
// fixtures) can serialize selectively.
type TraceEvent struct {
	// Tool is the tool name (from its [llm.ToolDef]).
	Tool string

	// Call is the inbound tool call as the LLM produced it. The
	// Arguments field is the raw JSON string the LLM emitted.
	Call llm.ToolCall

	// Result is the tool's response. Available only after Execute
	// returns; on the pre-call event the zero value is sent.
	Result kit.ToolResult

	// Phase is "before" for the pre-Execute notification or "after"
	// for the post-Execute notification.
	Phase TracePhase

	// Duration is the wall-clock time Execute took. Zero on the "before"
	// event.
	Duration time.Duration
}

// TracePhase identifies which side of [kit.Tool.Execute] a [TraceEvent]
// fires on.
type TracePhase string

const (
	// PhaseBefore fires synchronously immediately before Execute.
	PhaseBefore TracePhase = "before"
	// PhaseAfter fires synchronously immediately after Execute returns,
	// before the result is handed back to the agent loop.
	PhaseAfter TracePhase = "after"
)

// WithTracing returns a [kit.ToolDecorator] that invokes emit once
// before and once after every [kit.Tool.Execute] call.
//
// emit runs synchronously on the agent goroutine (Execute does too).
// Long-running observers should dispatch off the call stack — emit
// blocking blocks the agent. Panics inside emit propagate; the
// decorator does not recover. Production callers should defensively
// recover inside emit if their observer might panic on malformed
// data.
//
// Recommended chain position: outermost. Tracing the tool as the
// LLM sees it captures arguments and results faithfully; if other
// decorators rewrite the result, tracing observes the rewritten
// version when it runs outside them.
//
// Nil emit returns a no-op decorator (the wrapped tool passes through
// unchanged) — useful for conditionally-enabled tracing where the
// decorator slot is always allocated but the observer may be absent.
func WithTracing(emit func(TraceEvent)) kit.ToolDecorator {
	if emit == nil {
		return func(inner kit.Tool) kit.Tool { return inner }
	}
	return func(inner kit.Tool) kit.Tool {
		return &tracedTool{inner: inner, emit: emit}
	}
}

// tracedTool implements [kit.Tool] by wrapping Execute with emit
// callbacks. Definition delegates verbatim.
type tracedTool struct {
	inner kit.Tool
	emit  func(TraceEvent)
}

// Compile-time assertion that tracedTool satisfies [kit.Tool].
var _ kit.Tool = (*tracedTool)(nil)

// Definition delegates to the wrapped tool — tracing does not alter
// the schema the LLM sees.
func (t *tracedTool) Definition() llm.ToolDef {
	return t.inner.Definition()
}

// Execute emits PhaseBefore, runs the wrapped tool, then emits
// PhaseAfter with the captured duration and result.
func (t *tracedTool) Execute(ctx context.Context, call llm.ToolCall) kit.ToolResult {
	name := t.inner.Definition().Function.Name
	t.emit(TraceEvent{
		Tool:  name,
		Call:  call,
		Phase: PhaseBefore,
	})
	start := time.Now()
	result := t.inner.Execute(ctx, call)
	t.emit(TraceEvent{
		Tool:     name,
		Call:     call,
		Result:   result,
		Phase:    PhaseAfter,
		Duration: time.Since(start),
	})
	return result
}

// PromptGuidelines forwards the wrapped tool's prompt guidance
// (kit.PromptContributor). Tracing observes execution; it must not
// strip the tool's optional prompt-level surfaces.
func (t *tracedTool) PromptGuidelines() []string {
	return kit.ToolPromptGuidelines(t.inner)
}

// BindShell forwards kit.ShellBinder: the rebound inner tool stays
// traced. Returns a new wrapper; the receiver is unchanged.
func (t *tracedTool) BindShell(r dyncontext.Runner) kit.Tool {
	return &tracedTool{inner: kit.BindToolShell(t.inner, r), emit: t.emit}
}
