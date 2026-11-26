// Package permission decides whether a tool call may run: unasked in
// the autonomous mode, and by asking the person in the approve mode.
// The decision is deterministic: no model takes part in it.
package permission

import (
	"context"
	"strings"
	"sync"

	"github.com/latebit-io/nib/kit"
)

// Mode is how much a session does without asking.
type Mode int

const (
	// Autonomous runs tool calls without asking. It is the default.
	// Tools keep whatever refusals of their own they have.
	Autonomous Mode = iota
	// Approve asks the person before every call that is not exempt.
	Approve
)

// Decision is the person's answer to a request.
type Decision int

const (
	// Deny refuses the call. The model is told and can try something else.
	Deny Decision = iota
	// Allow lets this one call run.
	Allow
	// AllowAlways lets it run and stops asking about calls like it for
	// the rest of the session.
	AllowAlways
)

// Check is what a policy says about one call.
type Check struct {
	// Skip lets the call run without asking: it changes nothing.
	Skip bool
	// Detail is what the person is shown: the command, the path.
	Detail string
	// Reason says why the call is risky. Empty for an ordinary call.
	Reason string
	// Scope names what [AllowAlways] covers. Calls with the same tool
	// and scope are not asked about again. Empty means this tool, always.
	Scope string
}

// Policy inspects a call's raw JSON arguments.
type Policy func(args string) Check

// Unasked is the policy for a tool that only reads.
func Unasked(string) Check { return Check{Skip: true} }

// Request is one call the person is asked about.
type Request struct {
	// Tool is the name of the tool that wants to run.
	Tool string
	// Detail is what the call would do.
	Detail string
	// Reason says why the call is risky, when it is.
	Reason string
}

// Asker puts a request to the person and returns the answer. It is a
// callback, not an event: the run waits on it, and a frontend that does
// not provide one cannot leave the run hanging.
type Asker func(ctx context.Context, r Request) (Decision, error)

const (
	// rejected is what the model reads when the person says no.
	rejected = "The person rejected this call. Try a different approach or move on."
	// nobody is what it reads when the approve mode has no frontend.
	nobody = "This call needs the person's approval and this session has no way to ask. Do without it."
	// maxDetail bounds what is shown for a tool that has no policy.
	maxDetail = 200
)

// Gate applies the mode to every tool call. One Gate serves a session.
// What the person allows is remembered for the session only: a list
// kept in the project could be planted by the repository itself.
type Gate struct {
	mu       sync.Mutex
	mode     Mode
	asker    Asker
	policies map[string]Policy
	always   map[string]bool // tool + "\x00" + scope
	approved map[string]bool // call IDs the person allowed, until claimed
}

// New returns a Gate in the autonomous mode, with nobody to ask yet.
func New() *Gate {
	return &Gate{policies: map[string]Policy{}, always: map[string]bool{}, approved: map[string]bool{}}
}

// SetAsker sets who is asked in the approve mode. Without one, every
// call that would be asked about is refused.
func (g *Gate) SetAsker(a Asker) {
	g.mu.Lock()
	g.asker = a
	g.mu.Unlock()
}

// Register sets the policy for a tool, replacing an earlier one. A
// tool with no policy is asked about, showing its arguments.
func (g *Gate) Register(tool string, p Policy) {
	g.mu.Lock()
	g.policies[tool] = p
	g.mu.Unlock()
}

// SetMode switches the mode. It applies from the next tool call.
func (g *Gate) SetMode(m Mode) {
	g.mu.Lock()
	g.mode = m
	g.mu.Unlock()
}

// Mode returns the current mode.
func (g *Gate) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// Approved reports, once, whether the person allowed the call with
// this ID. A tool with a refusal of its own lifts it for such a call.
func (g *Gate) Approved(callID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ok := g.approved[callID]
	delete(g.approved, callID)
	return ok
}

// Hooks returns the hook that puts the gate in front of every call.
func (g *Gate) Hooks() kit.Hooks {
	return kit.Hooks{BeforeToolCall: g.before}
}

func (g *Gate) before(ctx context.Context, c kit.BeforeToolCallInput) (kit.BeforeToolCallResult, error) {
	g.mu.Lock()
	mode, policy, asker := g.mode, g.policies[c.Name], g.asker
	g.mu.Unlock()
	if mode == Autonomous {
		return kit.BeforeToolCallResult{}, nil
	}
	// A tool nobody described may do anything, so it is asked about.
	check := Check{Detail: preview(c.Args)}
	if policy != nil {
		check = policy(c.Args)
	}
	key := c.Name + "\x00" + check.Scope
	g.mu.Lock()
	known := g.always[key]
	g.mu.Unlock()
	switch {
	case check.Skip:
		return kit.BeforeToolCallResult{}, nil
	case known:
		return g.allow(c.CallID), nil
	case asker == nil:
		return kit.BeforeToolCallResult{Block: true, Reason: nobody}, nil
	}
	decision, err := asker(ctx, Request{Tool: c.Name, Detail: check.Detail, Reason: check.Reason})
	if err != nil {
		return kit.BeforeToolCallResult{}, err
	}
	switch decision {
	case AllowAlways:
		g.mu.Lock()
		g.always[key] = true
		g.mu.Unlock()
		return g.allow(c.CallID), nil
	case Allow:
		return g.allow(c.CallID), nil
	}
	return kit.BeforeToolCallResult{Block: true, Reason: rejected}, nil
}

// allow lets the call run and records the approval for [Gate.Approved].
func (g *Gate) allow(callID string) kit.BeforeToolCallResult {
	g.mu.Lock()
	g.approved[callID] = true
	g.mu.Unlock()
	return kit.BeforeToolCallResult{}
}

// preview shortens raw arguments to something a person can read.
func preview(args string) string {
	if len(args) <= maxDetail {
		return args
	}
	return strings.ToValidUTF8(args[:maxDetail], "") + "…"
}
