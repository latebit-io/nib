// Package hookspec parses the Claude Code `hooks/hooks.json` lifecycle-
// hook configuration into a validated, matchable [Config]. It is pure:
// parsing, validation, and matcher evaluation only — no execution and no
// engine wiring. Running a hook (shell command, JSON stdin → decision
// stdout) and emitting the lifecycle events that fire hooks are later
// verticals built on this foundation.
//
// A hooks file maps an [Event] (PreToolUse, PostToolUse, SessionStart,
// Stop, …) to a list of [Group]s. Each group has a tool-name regex
// matcher (empty = match all) and a list of [Hook]s to run when the event
// fires and the matcher matches. v1 models the `command` hook type fully;
// other types (http, mcp_tool, prompt, agent) are parsed and preserved so
// a converter can report them, but only command hooks are [Hook.Runnable].
package hookspec
