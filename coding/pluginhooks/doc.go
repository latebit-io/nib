// Package pluginhooks dispatches Claude Code plugin lifecycle hooks at
// nib's run-loop emit points. It is the coding-layer bridge between the
// pure hook model ([github.com/latebit-io/nib/kit/hookspec]), the command
// runner ([github.com/latebit-io/nib/kit/hookrun]), and the CC↔nib
// tool-name reconciliation ([github.com/latebit-io/nib/kit/hookmap]).
//
// A [Dispatcher] holds the parsed, trusted-plugin hook configs and a
// runner, and exposes one method per CC event. The coding agent calls
// PreToolUse / PostToolUse from its foundation before/after closures, and
// the remaining events (SessionStart, Stop, PreCompact, SubagentStop,
// UserPromptSubmit) from new coding-layer emit points — keeping the
// foundation [agent.Hooks] struct unchanged.
//
// Deny semantics are deliberate and load-bearing: a hook that objects
// maps to a BLOCK (PreToolUse) or an IsError result override
// (PostToolUse), NEVER to a returned Go error. A returned error in the
// foundation loop is terminal (it ends the whole run), so a plugin must
// never be able to kill a run by denying one tool call. Infrastructure
// failures (a hook that won't spawn, times out) fail OPEN in [hookrun]
// and are logged here, never converted to a deny.
//
// The zero value is not usable; construct with [New]. Methods are
// nil-receiver-safe so a caller that has no hooks configured can hold a
// nil *Dispatcher and call its methods unconditionally (every method is
// then a no-op that proceeds).
package pluginhooks
