package pluginstore

import (
	"regexp"
	"slices"
)

// varRef matches a ${NAME} or ${user_config.key} reference. The name is
// captured for lookup; an unmatched reference is left verbatim so the
// caller can report it rather than silently blanking it.
var varRef = regexp.MustCompile(`\$\{([A-Za-z0-9_.]+)\}`)

// Vars holds the values substituted into plugin component strings at
// import time. The CC variables that can be frozen at conversion live
// here; the ones that are genuinely runtime (user config the user has
// not yet supplied) are simply absent and reported by [expandVars].
type Vars struct {
	// PluginRoot is the absolute path of the installed plugin source
	// tree — the nib equivalent of ${CLAUDE_PLUGIN_ROOT}.
	PluginRoot string
	// PluginData is the absolute path of the plugin's persistent data
	// directory — the nib equivalent of ${CLAUDE_PLUGIN_DATA}.
	PluginData string
	// ProjectDir is the absolute project root (${CLAUDE_PROJECT_DIR}).
	// May be empty at import time; left unexpanded then.
	ProjectDir string
}

// lookup maps a CC variable name to its frozen value, reporting whether
// it is known. Both the CLAUDE_-prefixed and bare forms are accepted so
// converted plugins work whether the upstream wrote ${CLAUDE_PLUGIN_ROOT}
// or a future nib-native name.
func (v Vars) lookup(name string) (string, bool) {
	switch name {
	case "CLAUDE_PLUGIN_ROOT", "NIB_PLUGIN_ROOT":
		return v.PluginRoot, v.PluginRoot != ""
	case "CLAUDE_PLUGIN_DATA", "NIB_PLUGIN_DATA":
		return v.PluginData, v.PluginData != ""
	case "CLAUDE_PROJECT_DIR", "NIB_PROJECT_DIR":
		return v.ProjectDir, v.ProjectDir != ""
	default:
		return "", false
	}
}

// expandVars substitutes known ${...} references in s and returns the
// result plus the sorted, de-duplicated list of references it could not
// resolve (left verbatim). Unresolved references are not an error — they
// are surfaced in the convert report so the user knows a runtime value
// (e.g. ${user_config.token}) still needs wiring.
func expandVars(s string, v Vars) (out string, unresolved []string) {
	seen := map[string]struct{}{}
	out = varRef.ReplaceAllStringFunc(s, func(match string) string {
		name := varRef.FindStringSubmatch(match)[1]
		if val, ok := v.lookup(name); ok {
			return val
		}
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			unresolved = append(unresolved, name)
		}
		return match
	})
	// Stable order for deterministic reports/tests.
	slices.Sort(unresolved)
	return out, unresolved
}
