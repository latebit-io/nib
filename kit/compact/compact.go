// Package compact keeps tool output from filling the context: before
// each request, results already sent once are cut to a head, and in a
// long conversation older ones to a line. The transcript is not changed.
package compact

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

const (
	// capBytes is what a tool result is cut to after the request that
	// first carried it.
	capBytes = 4 << 10
	// markerReserve is the room kept for the marker within capBytes.
	markerReserve = 128
	// threshold is the estimated history size, in tokens, past which
	// older results are reduced to a line.
	threshold = 30_000
	// stubMinBytes is the smallest result worth reducing to a line.
	stubMinBytes = 200
)

// Hooks returns the hook that trims tool output before each request.
// exempt names tools whose results are never touched, such as an
// activated skill's instructions; nil exempts none.
func Hooks(exempt func(tool string) bool) kit.Hooks {
	t := &trimmer{exempt: exempt}
	return kit.Hooks{TransformContext: t.transform}
}

// trimmer holds how far the conversation has been reduced. The mark
// moves only when the history is too long again, so most requests send
// earlier messages unchanged and a provider's prompt cache stays valid.
type trimmer struct {
	exempt func(tool string) bool

	mu      sync.Mutex
	reduced int // messages before this index have their results reduced
}

func (t *trimmer) transform(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reduced > len(msgs) {
		t.reduced = 0 // the conversation was cleared or replaced
	}
	// Results after the last assistant message have not been sent yet.
	// They go whole, once: cutting them later rewrites nothing a cache holds.
	seen := lastAssistant(msgs)
	out := t.apply(msgs, seen)
	if t.reduced < seen && llm.EstimateMessageTokens(out, nil).History >= threshold {
		slog.Info("compact: reducing older tool output", "messages", seen, "before", t.reduced)
		t.reduced = seen
		out = t.apply(msgs, seen)
	}
	return out, nil
}

// lastAssistant returns the index of the last assistant message, or 0.
func lastAssistant(msgs []llm.Message) int {
	for i := len(msgs) - 1; i > 0; i-- {
		if msgs[i].Role == "assistant" {
			return i
		}
	}
	return 0
}

// apply returns msgs with the tool results before seen trimmed: those
// before the mark to a line, the rest to a head. msgs is not modified,
// and is returned as is when nothing changes.
func (t *trimmer) apply(msgs []llm.Message, seen int) []llm.Message {
	var out []llm.Message
	for i, m := range msgs[:seen] {
		if m.Role != "tool" {
			continue
		}
		var content string
		switch {
		case i < t.reduced && len(m.Content) > stubMinBytes:
			content = fmt.Sprintf("[%d bytes of output removed to save context; call the tool again if you need it]", len(m.Content))
		case len(m.Content) > capBytes:
			content = cut(m.Content)
		default:
			continue
		}
		if t.exempt != nil && t.exempt(toolOf(msgs[:i], m.ToolCallID)) {
			continue
		}
		if out == nil {
			out = make([]llm.Message, len(msgs))
			copy(out, msgs)
		}
		out[i].Content = content
	}
	if out == nil {
		return msgs
	}
	return out
}

// toolOf returns the name of the tool whose call has the given ID,
// looking back from the result. "" when the call is not there.
func toolOf(before []llm.Message, callID string) string {
	for i := len(before) - 1; i >= 0; i-- {
		for _, call := range before[i].ToolCalls {
			if call.ID == callID {
				return call.Function.Name
			}
		}
	}
	return ""
}

// cut keeps the head of content and says how much was dropped.
func cut(content string) string {
	end := capBytes - markerReserve
	// Back off to a rune boundary: half a rune would be invalid text.
	for end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	return fmt.Sprintf("%s\n[truncated: %d original bytes, %d kept; call the tool again for the full content]",
		content[:end], len(content), end)
}
