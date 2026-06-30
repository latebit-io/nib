package hookmap

import (
	"strings"

	"github.com/latebit-io/nib/kit/hookspec"
)

// nibToCC maps a nib builtin tool name to its Claude Code equivalent.
// Keys are the canonical nib names (lowercase, as the builtin registry
// keys them); values are CC's canonical tool names. Only tools with a
// genuine CC counterpart appear here — a nib tool with no entry (e.g.
// apply_patch, replace_file) matches a hook group only by its nib name.
//
// Lookups in either direction are case-insensitive (see [CCName] /
// [NibName]); the table is keyed in canonical case for readability.
var nibToCC = map[string]string{
	"write_file":     "Write",
	"edit_file":      "Edit",
	"read_file":      "Read",
	"bash":           "Bash",
	"glob":           "Glob",
	"search_project": "Grep",
	"list_files":     "LS",
}

// ccToNib is the reverse of [nibToCC], keyed by the lowercased CC name so
// lookups are case-insensitive. Built once at init from the single source
// of truth above to keep the two directions from drifting.
var ccToNib = func() map[string]string {
	m := make(map[string]string, len(nibToCC))
	for nib, cc := range nibToCC {
		m[strings.ToLower(cc)] = nib
	}
	return m
}()

// CCName returns the Claude Code alias for a nib builtin tool name. The
// lookup is case-insensitive. ok is false for a tool with no CC
// counterpart, whose hook groups match by nib name only.
func CCName(nibTool string) (cc string, ok bool) {
	cc, ok = nibToCC[strings.ToLower(nibTool)]
	return cc, ok
}

// NibName returns the nib builtin tool name for a Claude Code tool name.
// The lookup is case-insensitive. ok is false when CC names a tool nib
// has no builtin equivalent for.
func NibName(ccTool string) (nib string, ok bool) {
	nib, ok = ccToNib[strings.ToLower(ccTool)]
	return nib, ok
}

// Matches reports whether a hook group applies to a nib tool call. A CC
// matcher is written against CC names, so the group is tested against
// both the nib tool name and its CC alias (if any); a match on either
// fires the group. An empty matcher matches every tool (delegated to
// [hookspec.Group.Matches]).
//
// Name resolution is case-insensitive, but the group's matcher regex is
// honored exactly as authored — a CC plugin's matcher keeps CC regex
// semantics. Tools with no CC alias match by nib name only.
func Matches(g hookspec.Group, nibTool string) bool {
	if g.Matches(nibTool) {
		return true
	}
	if cc, ok := CCName(nibTool); ok && g.Matches(cc) {
		return true
	}
	return false
}
