// Package command provides the slash-command framework that any kit
// consumer can register commands into. The framework owns the
// dispatch contract; consumers own the commands themselves.
//
// Two interfaces split commands by trust:
//
//   - [HandlerCommand] is Go code with full session-mutation access
//     via [Session]. Built-ins and Go-defined extensions implement
//     this. Markdown cannot.
//   - [PromptCommand] is a pure text template. The framework's
//     dispatcher renders the template against the user's argument
//     string and submits the result through [Session.SubmitPrompt].
//     A markdown loader can construct only PromptCommands; promoting
//     a markdown command to handler-level requires writing Go.
//
// The trust boundary is the type, not a flag — a markdown loader has
// no path to register a HandlerCommand. That keeps the trust model
// reviewable in the type system rather than in convention.
//
// Frontend-shaped operations (clearing transcript, quitting) are
// NOT on Session and NOT in this package. They live in their owning
// frontend (e.g. tui/command/) and are registered by the binary's
// composition site (e.g. cmd/nib-code/main.go) into a single
// [Registry]. Domain-shaped operations (compaction, autonomy)
// likewise live in their owning consumer package (coding/command/).
// Kit owns only the framework plus [NewHelp].
package command

import (
	"context"

	"github.com/latebit-io/nib/kit/toolperm"
)

// Definition is the descriptive surface every command shares. It
// drives /help rendering, registration-time conflict resolution,
// and diagnostics.
type Definition struct {
	// Name is the canonical lowercase command name without the
	// leading slash. It must match [a-z0-9_-]+.
	Name string

	// Aliases are optional alternate names. Each must satisfy the
	// same character class as Name. The registry resolves aliases
	// to the canonical Name on lookup.
	Aliases []string

	// Description is the one-line text shown by /help.
	Description string

	// Source describes where this command came from — built-in,
	// project-local, user-global, or MCP-provided. Drives
	// shadowing precedence at registration time and is surfaced
	// in /help.
	Source Source

	// AllowedTools / DisallowedTools are the command's tool-permission
	// grants (the `allowed-tools` / `disallowed-tools` frontmatter).
	// Carried so an imported plugin command's grants survive; enforcement
	// of a per-command permission scope is a later milestone. Build a
	// matcher via [Definition.Permissions].
	AllowedTools    []string
	DisallowedTools []string
}

// Permissions compiles the command's allowed/disallowed tool grants into
// a [toolperm.Matcher]. Malformed grants are dropped rather than failing
// the whole command. A command with no allowed-tools yields a matcher
// that permits nothing.
func (d Definition) Permissions() *toolperm.Matcher {
	allow, _ := toolperm.ParseField(d.AllowedTools)
	deny, _ := toolperm.ParseField(d.DisallowedTools)
	return toolperm.New(allow, deny)
}

// Source identifies where a command originated. The kind drives
// shadowing precedence (project > global > MCP > builtin); the path
// is a free-form diagnostic pointer (package path for builtins,
// file path for markdown commands, server name for MCP).
type Source struct {
	// Kind classifies the origin for shadowing precedence.
	Kind SourceKind

	// Path is a human-readable pointer to the origin — package path
	// for builtins, file path for markdown, server name for MCP.
	// Free-form; intended for diagnostics, not for programmatic
	// dispatch.
	Path string
}

// SourceKind classifies a command's origin. Higher-precedence kinds
// shadow lower-precedence kinds at registration time without error;
// collisions within the same kind error.
type SourceKind int

const (
	// SourceBuiltin is for commands compiled into a binary. Lowest
	// precedence — any other source shadows a builtin.
	SourceBuiltin SourceKind = iota

	// SourceMCP is for commands exposed by an MCP server. Higher
	// precedence than builtin (the user installed the server) but
	// lower than user/project markdown (which the user authored
	// directly).
	SourceMCP

	// SourceGlobal is for commands authored as markdown under the
	// user's global config directory (e.g. ~/.config/nib/commands/).
	SourceGlobal

	// SourceProject is for commands authored as markdown under the
	// project root (e.g. .nib/commands/). Highest precedence —
	// project commands shadow everything else.
	SourceProject

	// SourcePlugin is for commands imported from a managed plugin's
	// converted tree. Ranked above MCP/builtin but below user-authored
	// markdown (project/global) so a user's own command always wins over
	// a third-party plugin command. Its iota position is irrelevant —
	// shadowing rank is set explicitly in precedenceOrder.
	SourcePlugin
)

// Command is the marker every registered command implements.
// Concrete commands implement [HandlerCommand] (Go code) or
// [PromptCommand] (text template). The dispatcher type-switches on
// the concrete type — there is no discriminator field on Definition.
type Command interface {
	// Definition returns the command's descriptive surface. It must
	// return a stable value across calls.
	Definition() Definition
}

// HandlerCommand is a command implemented in Go code. Its [Handle]
// method receives a [Session] for kit-level mutations (display
// inline, submit a synthesized prompt) and the user's raw argument
// string. Domain-specific mutations are NOT on Session — handlers
// that need them capture their domain dependency at construction
// (see coding/command/compact for the pattern).
type HandlerCommand interface {
	Command

	// Handle runs the command. Errors are wrapped in [CommandError]
	// by the dispatcher and surfaced to the frontend; the handler
	// itself need not wrap.
	Handle(ctx context.Context, sess Session, args string) error
}

// PromptCommand is a command whose body is a text template. The
// dispatcher calls [Render] with the user's argument string; the
// returned prompt is submitted to the agent through
// [Session.SubmitPrompt] as if the user had typed it. A
// PromptCommand has no access to Session — that is the
// architectural firewall that keeps markdown-loadable commands from
// silently inheriting session-mutation power.
type PromptCommand interface {
	Command

	// Render expands the template against args and returns the text
	// to submit. Template engines that fail (e.g. unbalanced
	// substitutions) return a non-nil error; the dispatcher wraps
	// it in [CommandError].
	Render(args string) (prompt string, err error)
}

// Completer is implemented by commands that offer argument
// autocomplete. Optional — frontends type-assert and skip if
// absent. Phase 1 ships zero implementations; the interface exists
// so frontends can build autocomplete without touching kit later.
type Completer interface {
	// Complete returns candidate completions for the given argument
	// prefix. Returning an empty slice means "no suggestions"; the
	// frontend renders that as silence.
	Complete(ctx context.Context, prefix string) []string
}
