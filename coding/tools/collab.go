package tools

import (
	"context"

	"github.com/latebit-io/junto/engine/event"
)

// Approver mediates the edit_file / replace_file approval flow. The
// implementation in `coding/agent` owns the approve channel and the
// validation pipeline; tools just submit a proposal and receive the
// final tool-result body the LLM should see.
//
// Propose runs the validation pipeline, sends the proposal to the
// frontend, blocks on approval, and returns the outcome message
// together with a flag indicating whether that outcome was a fatal
// failure (delivery timeout, agent cancel, channel closed). When
// isError is true the tool should surface the body as a tool error
// so the frontend can render it distinctively; otherwise the body
// is normal flow (validation recalibration, rejection note, applied
// notice).
type Approver interface {
	// Propose runs the proposal through the configured validation
	// pipeline and approval flow. The returned string is the body the
	// LLM should see (validation feedback, rejection note, applied
	// notice, or error message); the bool reports whether the call
	// failed with an error the LLM should surface as a tool error.
	Propose(ctx context.Context, p EditProposal) (body string, isError bool)
}

// Navigator publishes navigation requests to the frontend. The
// go_to_line tool calls this so navigation events flow on the same
// channel as every other agent event without the tool ever seeing the
// channel.
type Navigator interface {
	// Navigate emits a navigation request for the editor frontend.
	Navigate(ctx context.Context, nav event.AgentNavigate)
}

// FileCreator publishes file-created notifications. Used by write_file
// to let the frontend open or focus a freshly written file.
type FileCreator interface {
	// FileCreated emits a file-created event for the path.
	FileCreated(ctx context.Context, path string)
}

// TaskReviewer runs the post-task-completion pipeline (lint, smoke,
// style review, next-task hint). Invoked by the update_task tool when
// the LLM marks a task complete; the returned string is appended to
// the tool result so the LLM sees the verdict in-band.
type TaskReviewer interface {
	// OnComplete runs the review pipeline and returns the body to
	// append to the tool result.
	OnComplete(ctx context.Context, baseMessage string) string
}
