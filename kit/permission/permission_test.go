package permission

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/nib/kit"
)

// answers is an asker that gives the next prepared decision and keeps
// what it was asked.
type answers struct {
	next []Decision
	seen []Request
}

func (a *answers) ask(_ context.Context, r Request) (Decision, error) {
	a.seen = append(a.seen, r)
	d := a.next[0]
	a.next = a.next[1:]
	return d, nil
}

func call(t *testing.T, g *Gate, id, tool, args string) kit.BeforeToolCallResult {
	t.Helper()
	res, err := g.Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: id, Name: tool, Args: args})
	if err != nil {
		t.Fatalf("%s %s: %v", tool, args, err)
	}
	return res
}

// shell is a policy like a shell's: always-allow covers the exact command.
func shell(args string) Check {
	if args == "rm" {
		return Check{Detail: args, Reason: "destructive", Scope: args}
	}
	return Check{Detail: args, Scope: args}
}

// TestGate_Autonomous: nothing is asked and nothing is refused here;
// a tool keeps its own refusals, and no call counts as approved.
func TestGate_Autonomous(t *testing.T) {
	a := &answers{}
	g := New()
	g.SetAsker(a.ask)
	g.Register("bash", shell)
	if call(t, g, "1", "bash", "rm").Block || call(t, g, "2", "unknown", "{}").Block {
		t.Error("the autonomous mode must not refuse a call")
	}
	if len(a.seen) != 0 || g.Approved("1") {
		t.Errorf("asked %d times, call approved: %v; want neither", len(a.seen), g.Approved("1"))
	}
}

// TestGate_Approve: every call is asked about unless its policy says it
// only reads; the answer decides; always-allow covers one scope.
func TestGate_Approve(t *testing.T) {
	a := &answers{next: []Decision{Allow, Deny, AllowAlways, Allow}}
	g := New()
	g.SetAsker(a.ask)
	g.Register("bash", shell)
	g.Register("read_file", Unasked)
	g.SetMode(Approve)

	if call(t, g, "1", "bash", "rm").Block || !g.Approved("1") || g.Approved("1") {
		t.Error("an allowed call must run and count as approved, once")
	}
	if res := call(t, g, "2", "bash", "make"); !res.Block || res.Reason != rejected || g.Approved("2") {
		t.Errorf("denied call = %+v, want it refused and not approved", res)
	}
	if call(t, g, "3", "bash", "make").Block || call(t, g, "4", "bash", "make").Block || !g.Approved("4") {
		t.Error("an always-allowed command must run, and run again unasked")
	}
	if call(t, g, "5", "read_file", `{"path":"a"}`).Block {
		t.Error("a tool that only reads must run unasked")
	}
	long := strings.Repeat("x", 500)
	if call(t, g, "6", "mcp__srv__do", long).Block {
		t.Error("the unknown tool was allowed and must run")
	}
	if len(a.seen) != 4 {
		t.Fatalf("asked %d times, want 4 (not for the repeat or the read)", len(a.seen))
	}
	if first := a.seen[0]; first.Tool != "bash" || first.Detail != "rm" || first.Reason != "destructive" {
		t.Errorf("request = %+v, want the tool, the detail and the reason", first)
	}
	if last := a.seen[3]; last.Tool != "mcp__srv__do" || len(last.Detail) > maxDetail+4 {
		t.Errorf("a tool with no policy must be asked about with a short view of its arguments: %+v", last)
	}
}

// TestGate_NobodyToAsk: without a frontend the approve mode refuses
// instead of waiting, and an asker's error ends the call.
func TestGate_NobodyToAsk(t *testing.T) {
	g := New()
	g.Register("bash", shell)
	g.SetMode(Approve)
	if res := call(t, g, "1", "bash", "make"); !res.Block || res.Reason != nobody {
		t.Errorf("with nobody to ask: %+v, want a refusal that says so", res)
	}
	g.SetAsker(func(context.Context, Request) (Decision, error) { return Deny, context.Canceled })
	if _, err := g.Hooks().BeforeToolCall(context.Background(), kit.BeforeToolCallInput{CallID: "2", Name: "bash", Args: "make"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the asker's error", err)
	}
}
