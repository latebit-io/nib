// Package truncation recovers when a provider cuts a response off at
// its output limit: the unfinished tool calls are refused, the limit is
// raised where the provider allows it, and the turn is tried again.
package truncation

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

const (
	// maxRetries is how many cut-off responses in a row are retried.
	maxRetries = 2
	// firstEscalation is the output limit a first retry asks for at least.
	firstEscalation = 32768
	// ceiling is the highest output limit a retry asks for.
	ceiling = 65536
)

const (
	refused = "Not executed: your response was cut off at the output limit before this call was complete. Send it again; if it is large, split it into smaller calls."
	nudge   = "Your last response was cut off at the output limit. Continue from where it stopped."
)

// Hooks returns the recovery hook. limit, when not nil, is the output
// limit to raise before a retry, usually the provider itself.
func Hooks(limit llm.OutputCapEscalator) kit.Hooks {
	return kit.Hooks{
		OnTruncated: func(_ context.Context, in kit.TruncationInput) (kit.TruncationResult, error) {
			if in.Consecutive > maxRetries {
				// The loop reports the error and answers the open calls.
				return kit.TruncationResult{}, fmt.Errorf("abandoning the turn after %d responses in a row were cut off at the output limit", in.Consecutive)
			}
			raised := 0
			if limit != nil {
				raised = escalated(limit.MaxTokens())
				limit.SetMaxTokens(raised)
			}
			slog.Info("truncation: retrying a cut-off response", "attempt", in.Consecutive, "output_limit", raised)
			// The calls are never run: their arguments may be half written.
			// Each still needs a reply, or the transcript is invalid.
			msgs := make([]llm.Message, 0, len(in.ToolCalls))
			for _, c := range in.ToolCalls {
				msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: c.ID, Content: refused})
			}
			if len(msgs) == 0 {
				msgs = []llm.Message{{Role: "user", Content: nudge}}
			}
			return kit.TruncationResult{Retry: true, Messages: msgs}, nil
		},
	}
}

// escalated returns the output limit to try after one of cur was hit:
// double, at least the first step, at most the ceiling. A limit already
// above the ceiling is kept: recovery must never give the model less room.
func escalated(cur int) int {
	if cur >= ceiling {
		return cur
	}
	return min(max(cur*2, firstEscalation), ceiling)
}
