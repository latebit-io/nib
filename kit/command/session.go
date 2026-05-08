package command

import "context"

// Session is the kit-level mutation surface a [HandlerCommand] sees
// during dispatch. Frontends implement Session; the [Registry]
// threads it through [Registry.Dispatch].
//
// The surface is deliberately minimal — only operations every kit
// consumer can implement. Frontend-shaped concerns (clearing a
// transcript, quitting a process, opening a URL) are NOT on
// Session. Commands that need them live in the frontend's own
// package and capture their dependencies at construction.
//
// Adding methods to Session is non-breaking for existing frontends
// when done via embedded interfaces (`type SessionV2 interface {
// Session; ... }`); removing or renaming is breaking. Resist
// convenience methods — answer them with frontend-side commands
// instead.
type Session interface {
	// Display renders text without involving the LLM. /help uses
	// this; any handler that wants to surface a result inline uses
	// this. The frontend decides how to render — the agent pane
	// in the TUI, stdout in nibster, etc.
	Display(text string)

	// SubmitPrompt synthesizes a user message and submits it to
	// the agent as if the user had typed it. PromptCommand uses
	// this path under the hood; HandlerCommand may use it for
	// template-with-side-effect patterns. The error is propagated
	// when the frontend cannot accept the prompt (agent shutting
	// down, no agent attached, etc.).
	SubmitPrompt(ctx context.Context, text string) error
}
