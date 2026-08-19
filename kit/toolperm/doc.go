// Package toolperm evaluates Claude Code style tool-permission grants —
// the `allowed-tools` / `disallowed-tools` frontmatter that commands,
// skills, agents, and hooks carry — against a concrete tool invocation.
//
// A grant is a [Rule]: a tool name with an optional argument glob, e.g.
// `Bash(git *)`, `Write(/etc/*)`, or a bare `Read` (any argument). A
// [Matcher] combines an allow set and a deny set and answers a single
// question — may this tool run with these arguments? Deny always wins,
// and an empty allow set grants nothing (the sandbox default: a plugin
// artifact may use only the tools it explicitly lists).
//
// The package is pure: parsing and matching, no I/O and no execution. It
// is the shared evaluator the trust/shell gates build on (skill loader,
// dyncontext, cmdallow), so the permission semantics live in one
// table-tested place rather than being re-derived at each call site.
package toolperm
