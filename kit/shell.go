package kit

import "github.com/latebit-io/nib/kit/dyncontext"

// ShellBinder is the optional interface a [Tool] implements when
// invoking it may run shell on its own — today the skill tool, whose
// dynamic-context directives (“ !`cmd` “) execute at invocation. Such
// a tool ships with a plain [dyncontext.ShellRunner]; an agent that
// owns an approval, allowlist, or grant surface for shell rebinds the
// tool so directives run through that surface instead of around it.
//
// BindShell returns a rebound copy and leaves the receiver untouched:
// one discovered tool value is commonly shared between a parent agent
// and its children, each of which must bind its own gate.
type ShellBinder interface {
	// BindShell returns a copy of the tool whose shell executes via r.
	BindShell(r dyncontext.Runner) Tool
}

// BindToolShell rebinds t's shell to r when the concrete type
// implements [ShellBinder]; otherwise t is returned unchanged. Agents
// call this per extra tool at registration, passing a runner over
// their own bash tool.
func BindToolShell(t Tool, r dyncontext.Runner) Tool {
	if b, ok := t.(ShellBinder); ok {
		return b.BindShell(r)
	}
	return t
}
