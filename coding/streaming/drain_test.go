package streaming

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
)

// TestDrain_ResetsThinkStateOnReturn locks in that Drain never leaks
// <think>...</think> state across stream boundaries. An unclosed
// <think> block at stream end used to keep inThink=true, causing the
// next turn's stripThinkTags to silently drop real output until a
// stray closing tag (which may never arrive) was seen.
func TestDrain_ResetsThinkStateOnReturn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		events  []llm.StreamEvent
		wantErr error // nil for Done-event paths, ErrClosedEarly for EOF
	}{
		{
			name: "clean think block closes state",
			events: []llm.StreamEvent{
				{Token: "<think>reasoning</think>answer"},
				{Done: true},
			},
		},
		{
			name: "unclosed think block at Done",
			events: []llm.StreamEvent{
				{Token: "<think>reasoning without close"},
				{Done: true, Truncated: true},
			},
		},
		{
			name: "channel closed mid-think (EOF path)",
			events: []llm.StreamEvent{
				{Token: "<think>incomplete"},
			},
			wantErr: ErrClosedEarly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sent := make([]event.Event, 0, 4)
			send := func(ev event.Event) { sent = append(sent, ev) }

			ch := make(chan llm.StreamEvent, len(tc.events)+1)
			for _, e := range tc.events {
				ch <- e
			}
			close(ch) // safe for both paths — Done-event tests still read their event first

			thinkState := true // start dirty to prove defer reset fires
			_, err := Drain(context.Background(), ch, &thinkState, send)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Drain error = %v, want %v", err, tc.wantErr)
			}
			if thinkState {
				t.Error("thinkState should be false after Drain returns")
			}
		})
	}
}

// TestDrain_ResetsThinkStateOnCtxCancel covers the remaining return
// path — ctx cancellation mid-think.
func TestDrain_ResetsThinkStateOnCtxCancel(t *testing.T) {
	t.Parallel()
	send := func(event.Event) {}

	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Token: "<think>opening"}
	// Do not close — force Drain to block until ctx cancels.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the select races to ctx.Done

	thinkState := true
	// ctx cancellation has its own handling path in processLLMTurn
	// (via ctx.Err() check), so Drain returns nil on this path —
	// see the function's doc comment.
	_, err := Drain(ctx, ch, &thinkState, send)
	if err != nil {
		t.Errorf("Drain error on ctx cancel = %v, want nil", err)
	}
	if thinkState {
		t.Error("thinkState should be false after Drain returns on ctx cancel")
	}
}

// TestDrain_StripsThinkTagsSplitAcrossChunks verifies the cross-
// boundary case the original implementation got wrong: a tag whose
// head ("<thi") and tail ("nk>") arrive in separate StreamEvent
// chunks must still be treated as a single tag, with the entire
// think block suppressed. Pre-fix this leaked the whole think
// block to the Sender because [strings.Index] returned -1 for the
// partial in chunk 1 and the matching closer in chunk 2 had no
// recognized opener.
//
// Five sub-cases lock the matrix: opener split at every position
// inside `<think>`, closer split at every position inside
// `</think>`, and a few interleavings.
func TestDrain_StripsThinkTagsSplitAcrossChunks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tokens []string
	}{
		{"opener split mid-tag (4-3)", []string{"Hello <thi", "nk>private</think>world."}},
		{"opener split at first char (1-6)", []string{"Hello <", "think>private</think>world."}},
		{"opener split at last char (6-1)", []string{"Hello <think", ">private</think>world."}},
		{"closer split mid-tag", []string{"Hello <think>private</thi", "nk>world."}},
		{"both split", []string{"Hello <thi", "nk>private</thi", "nk>world."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan llm.StreamEvent, len(tc.tokens)+1)
			for _, tok := range tc.tokens {
				ch <- llm.StreamEvent{Token: tok}
			}
			ch <- llm.StreamEvent{Done: true}
			close(ch)

			var sentText strings.Builder
			send := func(ev event.Event) {
				if tok, ok := ev.(event.AgentToken); ok {
					sentText.WriteString(tok.Text)
				}
			}

			thinkState := false
			res, err := Drain(context.Background(), ch, &thinkState, send)
			if err != nil {
				t.Fatalf("Drain err = %v, want nil", err)
			}
			if res.Content != "Hello world." {
				t.Errorf("Result.Content = %q, want %q", res.Content, "Hello world.")
			}
			// The Sender must NEVER see the secret content or any raw tag
			// fragments. Anything containing "private" or partial-tag
			// markers indicates the leak this test guards against.
			if strings.Contains(sentText.String(), "private") {
				t.Errorf("Sender leaked think-block content: %q", sentText.String())
			}
			for _, frag := range []string{"<think", "</think", "<thi", "</thi"} {
				if strings.Contains(sentText.String(), frag) {
					t.Errorf("Sender saw partial tag fragment %q in output: %q",
						frag, sentText.String())
				}
			}
		})
	}
}

// TestDrain_TrailingPartialTag_FlushesAtStreamEnd verifies that a
// stream ending with bytes that LOOK like the start of an opener
// but never materialize (e.g. "<thing") emits those bytes verbatim
// when Done arrives. The residue mechanism must not silently swallow
// content on the false-alarm path.
func TestDrain_TrailingPartialTag_FlushesAtStreamEnd(t *testing.T) {
	t.Parallel()
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Token: "Result: <"}
	ch <- llm.StreamEvent{Token: "thing>"} // "<thing>" — looks like opener, isn't
	ch <- llm.StreamEvent{Done: true}
	close(ch)

	var sent strings.Builder
	send := func(ev event.Event) {
		if tok, ok := ev.(event.AgentToken); ok {
			sent.WriteString(tok.Text)
		}
	}

	thinkState := false
	res, err := Drain(context.Background(), ch, &thinkState, send)
	if err != nil {
		t.Fatalf("Drain err = %v, want nil", err)
	}
	if res.Content != "Result: <thing>" {
		t.Errorf("Result.Content = %q, want %q (false-alarm bytes must flush)",
			res.Content, "Result: <thing>")
	}
	if sent.String() != "Result: <thing>" {
		t.Errorf("Sender output = %q, want %q", sent.String(), "Result: <thing>")
	}
}

// TestDrain_TrailingPartialTagInThink_DoesNotLeak verifies that an
// open think block ending mid-stream (with a partial closer like
// "</thi") does NOT flush the held bytes — we never exited the
// think block, so the residue must be suppressed.
func TestDrain_TrailingPartialTagInThink_DoesNotLeak(t *testing.T) {
	t.Parallel()
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Token: "ok <think>secret</thi"}
	ch <- llm.StreamEvent{Done: true}
	close(ch)

	var sent strings.Builder
	send := func(ev event.Event) {
		if tok, ok := ev.(event.AgentToken); ok {
			sent.WriteString(tok.Text)
		}
	}

	thinkState := false
	res, err := Drain(context.Background(), ch, &thinkState, send)
	if err != nil {
		t.Fatalf("Drain err = %v, want nil", err)
	}
	if res.Content != "ok " {
		t.Errorf("Result.Content = %q, want %q (in-think residue must not flush)",
			res.Content, "ok ")
	}
	if strings.Contains(sent.String(), "secret") || strings.Contains(sent.String(), "</thi") {
		t.Errorf("Sender leaked think-block content or partial closer: %q", sent.String())
	}
}

// TestDrain_ForwardsTokensAndCollectsTerminalState verifies the
// happy-path: text tokens stream out via the Sender, the terminal
// Done event populates the Result fields, and Content reflects the
// stripped concatenation.
func TestDrain_ForwardsTokensAndCollectsTerminalState(t *testing.T) {
	t.Parallel()
	ch := make(chan llm.StreamEvent, 4)
	ch <- llm.StreamEvent{Token: "Hello, "}
	ch <- llm.StreamEvent{Token: "<think>plotting</think>"}
	ch <- llm.StreamEvent{Token: "world."}
	ch <- llm.StreamEvent{
		Done:      true,
		Truncated: false,
		ToolCalls: []llm.ToolCall{{ID: "call-1"}},
		Usage:     &llm.Usage{PromptTokens: 10, CompletionTokens: 5},
	}
	close(ch)

	var tokens []string
	send := func(ev event.Event) {
		if tok, ok := ev.(event.AgentToken); ok {
			tokens = append(tokens, tok.Text)
		}
	}

	thinkState := false
	res, err := Drain(context.Background(), ch, &thinkState, send)
	if err != nil {
		t.Fatalf("Drain err = %v, want nil", err)
	}
	if res.Content != "Hello, world." {
		t.Errorf("Result.Content = %q, want %q", res.Content, "Hello, world.")
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call-1" {
		t.Errorf("Result.ToolCalls = %+v, want one call with ID call-1", res.ToolCalls)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 10 {
		t.Errorf("Result.Usage = %+v, want PromptTokens=10", res.Usage)
	}
	// Tokens forwarded to Sender should be the cleaned (think-stripped)
	// text — never the raw <think>...</think> bytes.
	for _, tok := range tokens {
		if strings.Contains(tok, "<think>") || strings.Contains(tok, "</think>") {
			t.Errorf("Sender saw raw think-tag bytes: %q", tok)
		}
	}
}
