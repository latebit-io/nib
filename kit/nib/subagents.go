package nib

import (
	"errors"
	"slices"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/permission"
	"github.com/latebit-io/nib/kit/subagent"
)

// child is what a subagent is built from, as compose last left it: the
// session's tools (without the delegation tools), hooks and project text.
type child struct {
	tools  []kit.Tool
	hooks  kit.Hooks
	prompt string
}

// delegation builds the tools that hand a task to a subagent: one per
// agent definition and one per forked skill. They need no approval of
// their own: each call the child makes goes through the gate.
func (a *Assembly) delegation(defs []agentdef.Definition, providerFor func(model, effort string) (llm.Provider, error)) {
	host := subagent.Host{
		Provider: func(model, effort string) (llm.Provider, error) {
			if model == "" && effort == "" {
				return a.meter, nil
			}
			if providerFor == nil {
				return nil, errors.New("this session cannot choose another model")
			}
			p, err := providerFor(model, effort)
			if err != nil {
				return nil, err
			}
			// Counted in the session, budget included.
			return a.meter.Branch(p), nil
		},
		Tools: func() []kit.Tool { return a.child.Load().tools },
		Hooks: func(p llm.Provider) kit.Hooks {
			limit, _ := p.(llm.OutputCapEscalator) // nil: truncation retries without raising a limit
			return kit.Merge(machinery(limit, a.Permission, a.limits), kit.Toolset{Hooks: a.child.Load().hooks}).Hooks
		},
		Prompt: func(persona string) string {
			return strings.TrimSpace(strings.TrimSpace(persona) + "\n\n" + a.child.Load().prompt)
		},
		OnStop: a.Hooks.SubagentStop,
	}
	agents, errs := subagent.AgentTools(defs, host)
	a.Warnings = append(a.Warnings, errs...)
	forks, errs := subagent.ForkTools(a.skills.Forking, host)
	a.Warnings = append(a.Warnings, errs...)
	a.delegates = append(agents, forks...)
	for _, t := range a.delegates {
		a.Permission.Register(t.Definition().Function.Name, permission.Unasked)
	}
	// The manifest lists the agents that start, with where each came from.
	for _, t := range agents {
		name := t.Definition().Function.Name
		i := slices.IndexFunc(defs, func(d agentdef.Definition) bool { return subagent.ToolNamePrefix+d.Name == name })
		source := string(defs[i].Source)
		if defs[i].PluginID != "" {
			source = "plugin:" + defs[i].PluginID
		}
		a.agentRows = append(a.agentRows, kit.Plugin{Kind: kit.KindAgent, Name: name, Description: defs[i].Description, Source: source})
	}
}
