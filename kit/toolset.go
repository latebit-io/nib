package kit

import (
	"context"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/command"
)

// Toolset is a composable bundle of tools, hooks, and slash commands.
// Construct with a struct literal; combine with [Merge]. The zero value
// is a valid empty toolset that contributes nothing when merged.
//
// Toolset is the canonical plug-in registration unit. A plug-in package
// (bundled or third-party) exposes a single Plugins() Toolset function;
// the composition root composes them via Merge.
type Toolset struct {
	// Tools are the tool implementations in this bundle. Order is
	// preserved through Merge for stable LLM tool-list ordering.
	Tools []Tool

	// Hooks are the lifecycle hooks this bundle contributes. When
	// merged, each non-nil hook field chains with hooks from other
	// toolsets in Merge order.
	Hooks Hooks

	// Commands are the slash commands this bundle contributes. Order
	// is preserved through Merge. Merge does NOT deduplicate; the
	// composition root registers them into a [command.Registry], which
	// owns precedence-based shadowing (project > global > MCP > builtin)
	// and same-kind collision detection. Plug-ins ship commands at the
	// SourceBuiltin kind; markdown loaders supply higher-precedence
	// kinds separately.
	Commands []command.Command
}

// Merge combines toolsets into a single Toolset. Tools and Commands are
// concatenated in order. Hook functions chain sequentially — see each
// hook field's chaining contract below. Merge does NOT deduplicate
// tools or commands; tool dedup happens at [New] time, and command
// dedup is the [command.Registry]'s responsibility (precedence model).
//
// Merge is associative: Merge(a, Merge(b, c)) equals Merge(a, b, c).
// The zero-value Toolset contributes nothing.
func Merge(sets ...Toolset) Toolset {
	var (
		tools    []Tool
		commands []command.Command
	)
	for _, s := range sets {
		tools = append(tools, s.Tools...)
		commands = append(commands, s.Commands...)
	}

	return Toolset{
		Tools:    tools,
		Hooks:    mergeHooks(sets),
		Commands: commands,
	}
}

// mergeHooks chains every non-nil hook across all toolsets.
func mergeHooks(sets []Toolset) Hooks {
	var (
		befores     []func(context.Context, BeforeToolCallContext) (BeforeToolCallResult, error)
		afters      []func(context.Context, AfterToolCallContext) (AfterToolCallResult, error)
		transforms  []func(context.Context, []llm.Message) ([]llm.Message, error)
		steerings   []func(context.Context) ([]llm.Message, error)
		followUps   []func(context.Context) ([]llm.Message, error)
		beforeParks []func(context.Context) (event.AgentParked, error)
		truncs      []func(context.Context, TruncationContext) (TruncationResult, error)
	)
	for _, s := range sets {
		if s.Hooks.BeforeToolCall != nil {
			befores = append(befores, s.Hooks.BeforeToolCall)
		}
		if s.Hooks.AfterToolCall != nil {
			afters = append(afters, s.Hooks.AfterToolCall)
		}
		if s.Hooks.TransformContext != nil {
			transforms = append(transforms, s.Hooks.TransformContext)
		}
		if s.Hooks.GetSteeringMessages != nil {
			steerings = append(steerings, s.Hooks.GetSteeringMessages)
		}
		if s.Hooks.GetFollowUpMessages != nil {
			followUps = append(followUps, s.Hooks.GetFollowUpMessages)
		}
		if s.Hooks.BeforePark != nil {
			beforeParks = append(beforeParks, s.Hooks.BeforePark)
		}
		if s.Hooks.OnTruncated != nil {
			truncs = append(truncs, s.Hooks.OnTruncated)
		}
	}

	return Hooks{
		BeforeToolCall:      chainBeforeToolCall(befores),
		AfterToolCall:       chainAfterToolCall(afters),
		TransformContext:    chainTransformContext(transforms),
		GetSteeringMessages: chainGetMessages(steerings),
		GetFollowUpMessages: chainGetMessages(followUps),
		BeforePark:          chainBeforePark(beforeParks),
		OnTruncated:         chainOnTruncated(truncs),
	}
}

// chainBeforePark chains BeforePark hooks. Each runs in order; the
// returned [event.AgentParked] OR-aggregates the Finished bool — any
// hook signalling "finished" carries through. Errors short-circuit.
// Returns nil when fns is empty.
func chainBeforePark(fns []func(context.Context) (event.AgentParked, error)) func(context.Context) (event.AgentParked, error) {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(ctx context.Context) (event.AgentParked, error) {
		var merged event.AgentParked
		for _, fn := range fns {
			res, err := fn(ctx)
			if res.Finished {
				merged.Finished = true
			}
			if err != nil {
				return merged, err
			}
		}
		return merged, nil
	}
}

// chainBeforeToolCall chains before-tool-call hooks. First Block=true
// short-circuits; errors short-circuit. Returns nil when fns is empty.
func chainBeforeToolCall(fns []func(context.Context, BeforeToolCallContext) (BeforeToolCallResult, error)) func(context.Context, BeforeToolCallContext) (BeforeToolCallResult, error) {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(ctx context.Context, c BeforeToolCallContext) (BeforeToolCallResult, error) {
		for _, fn := range fns {
			res, err := fn(ctx, c)
			if err != nil {
				return res, err
			}
			if res.Block {
				return res, nil
			}
		}
		return BeforeToolCallResult{}, nil
	}
}

// chainAfterToolCall chains after-tool-call hooks. Later non-nil
// pointer fields (Content, IsError) override earlier. Terminate is
// OR'd. Errors short-circuit. Returns nil when fns is empty.
func chainAfterToolCall(fns []func(context.Context, AfterToolCallContext) (AfterToolCallResult, error)) func(context.Context, AfterToolCallContext) (AfterToolCallResult, error) {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(ctx context.Context, c AfterToolCallContext) (AfterToolCallResult, error) {
		var merged AfterToolCallResult
		for _, fn := range fns {
			res, err := fn(ctx, c)
			if res.Content != nil {
				merged.Content = res.Content
			}
			if res.IsError != nil {
				merged.IsError = res.IsError
			}
			if res.Terminate {
				merged.Terminate = true
			}
			if err != nil {
				return merged, err
			}
		}
		return merged, nil
	}
}

// chainTransformContext pipelines transform hooks. Each receives the
// prior output. nil return from a stage passes input through. Errors
// short-circuit. Returns nil when fns is empty.
func chainTransformContext(fns []func(context.Context, []llm.Message) ([]llm.Message, error)) func(context.Context, []llm.Message) ([]llm.Message, error) {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(ctx context.Context, msgs []llm.Message) ([]llm.Message, error) {
		for _, fn := range fns {
			out, err := fn(ctx, msgs)
			if err != nil {
				return nil, err
			}
			if out != nil {
				msgs = out
			}
		}
		return msgs, nil
	}
}

// chainGetMessages concatenates message slices from all hooks. Errors
// short-circuit. Returns nil when fns is empty. Used for both
// GetSteeringMessages and GetFollowUpMessages.
func chainGetMessages(fns []func(context.Context) ([]llm.Message, error)) func(context.Context) ([]llm.Message, error) {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(ctx context.Context) ([]llm.Message, error) {
		var all []llm.Message
		for _, fn := range fns {
			msgs, err := fn(ctx)
			if err != nil {
				return nil, err
			}
			all = append(all, msgs...)
		}
		return all, nil
	}
}

// chainOnTruncated chains truncation hooks. Each fires in order; the
// last non-zero result wins. Errors short-circuit. Returns nil when
// fns is empty.
func chainOnTruncated(fns []func(context.Context, TruncationContext) (TruncationResult, error)) func(context.Context, TruncationContext) (TruncationResult, error) {
	switch len(fns) {
	case 0:
		return nil
	case 1:
		return fns[0]
	}
	return func(ctx context.Context, c TruncationContext) (TruncationResult, error) {
		var last TruncationResult
		for _, fn := range fns {
			res, err := fn(ctx, c)
			if res.Retry || len(res.Messages) > 0 {
				last = res
			}
			if err != nil {
				return last, err
			}
		}
		return last, nil
	}
}

// deduplicateTools returns the unique tools from the input slice.
// First tool with a given name (case-insensitive) wins; later
// duplicates are logged at Warn level and dropped.
func deduplicateTools(tools []Tool) []Tool {
	seen := make(map[string]bool, len(tools))
	deduped := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			deduped = append(deduped, t)
			continue
		}
		name := strings.ToLower(t.Definition().Function.Name)
		if seen[name] {
			slog.Warn("kit: duplicate tool dropped", "name", name)
			continue
		}
		seen[name] = true
		deduped = append(deduped, t)
	}
	return deduped
}
