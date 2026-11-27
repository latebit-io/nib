package hookmap

import (
	"strings"

	"github.com/latebit-io/nib/kit/hookspec"
)

// pairs maps nib builtin tool names to their Claude Code equivalents. A
// nib tool with no entry (e.g. apply_patch) matches a hook group only by
// its nib name. Lookups either way are case-insensitive.
var pairs = []struct{ nib, cc string }{
	{"write_file", "Write"}, {"edit_file", "Edit"}, {"read_file", "Read"}, {"bash", "Bash"},
	{"glob", "Glob"}, {"search_project", "Grep"},
	// Two generations of the listing tool share LS; the parked agent's first.
	{"list_files", "LS"}, {"list_dir", "LS"},
}

// nibToCC and ccToNib are derived from pairs, so the two directions
// cannot drift apart.
var nibToCC, ccToNib = func() (map[string]string, map[string][]string) {
	forward, reverse := map[string]string{}, map[string][]string{}
	for _, p := range pairs {
		forward[p.nib] = p.cc
		key := strings.ToLower(p.cc)
		reverse[key] = append(reverse[key], p.nib)
	}
	return forward, reverse
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
	names := ccToNib[strings.ToLower(ccTool)]
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}

// NibNames returns every nib tool name a Claude Code alias stands for:
// two generations of nib tools share some aliases.
func NibNames(ccTool string) []string {
	return ccToNib[strings.ToLower(ccTool)]
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
