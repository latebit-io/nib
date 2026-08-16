package skill

import "github.com/latebit-io/nib/kit"

// Plugins renders a [Discover] Result into kit.Plugin entries for a
// `--plugins` manifest: each loaded skill (tagged with its source
// layer), each untrusted plugin skill refused, and each shadowed skill.
// Centralized here so every composition root surfaces skills — and
// their refusal/shadow reasons — identically; callers skip the
// [ToolNamePrefix]-named tools in their generic tool loop and append
// these instead.
func Plugins(r Result) []kit.Plugin {
	out := make([]kit.Plugin, 0, len(r.Loaded)+len(r.Skipped)+len(r.Shadowed))
	for _, s := range r.Loaded {
		out = append(out, kit.Plugin{
			Kind:        kit.KindTool,
			Name:        ToolNamePrefix + s.Name,
			Description: s.Description,
			Source:      string(s.Source),
		})
	}
	for _, s := range r.Skipped {
		out = append(out, kit.Plugin{
			Kind:        kit.KindTool,
			Name:        ToolNamePrefix + s.Name,
			Description: "refused — plugin shell/fork skill from an untrusted plugin (run /plugin trust)",
			Source:      string(s.Source),
		})
	}
	for _, s := range r.Shadowed {
		out = append(out, kit.Plugin{
			Kind:        kit.KindTool,
			Name:        ToolNamePrefix + s.Name,
			Description: "shadowed by a higher-precedence skill of the same name",
			Source:      string(s.Source),
		})
	}
	return out
}
