package kit

import (
	"context"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
)

// Toolset is a composable bundle of tools and hooks. Construct with a
// struct literal; combine with [Merge]. The zero value is a valid empty
// toolset that contributes nothing when merged.
type Toolset struct {
	// Tools are the tool implementations in this bundle. Order is
	// preserved through Merge for stable LLM tool-list ordering.
	Tools []Tool

	// Hooks are the lifecycle hooks this bundle contributes. When
	// merged, each non-nil hook field chains with hooks from other
	// toolsets in Merge order.
	Hooks Hooks
}

// Merge combines toolsets into a single Toolset. Tools are concatenated
// in order. Hook functions chain sequentially — see each hook field's
// chaining contract below. Merge does NOT deduplicate tools; that
// happens at [New] time where builtin-vs-extra precedence applies.
//
// Merge is associative: Merge(a, Merge(b, c)) equals Merge(a, b, c).
// The zero-value Toolset contributes nothing.
func Merge(sets ...Toolset) Toolset {
	var tools []Tool
	for _, s := range sets {
		tools = append(tools, s.Tools...)
	}

	return Toolset{
		Tools: tools,
		Hooks: mergeHooks(sets),
	}
}

// mergeHooks chains every non-nil hook across all toolsets.
func mergeHooks(sets []Toolset) Hooks {
	var (
		befores    []func(context.Context, BeforeToolCallContext) (BeforeToolCallResult, error)
		afters     []func(context.Context, AfterToolCallContext) (AfterToolCallResult, error)
		transforms []func(context.Context, []llm.Message) ([]llm.Message, error)
		steerings  []func(context.Context) ([]llm.Message, error)
		followUps  []func(context.Context) ([]llm.Message, error)
		truncs     []func(context.Context, TruncationContext) (TruncationResult, error)
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
		OnTruncated:         chainOnTruncated(truncs),
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
			if err != nil {
				return merged, err
			}
			if res.Content != nil {
				merged.Content = res.Content
			}
			if res.IsError != nil {
				merged.IsError = res.IsError
			}
			if res.Terminate {
				merged.Terminate = true
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
			if err != nil {
				return last, err
			}
			if res.Retry || len(res.Messages) > 0 {
				last = res
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
