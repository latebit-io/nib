package agent

import (
	"context"
	"strings"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/approval"
	"github.com/latebit-io/nib/kit/cmdallow"
)

// stubPropose returns a proposeCommandFunc with a canned decision and
// records the command it was asked about.
func stubPropose(approved bool, body string, isError bool, got *string) proposeCommandFunc {
	return func(_ context.Context, _, command, _ string) (bool, string, bool) {
		if got != nil {
			*got = command
		}
		return approved, body, isError
	}
}

func TestCommandApprovalGate_Execute(t *testing.T) {
	t.Run("approved command delegates", func(t *testing.T) {
		calls := 0
		var proposed string
		gate := commandApprovalGate{
			inner:   stubTool{name: "bash", calls: &calls, result: upagent.ToolResult{Content: "ok"}},
			propose: stubPropose(true, "", false, &proposed),
		}
		res := gate.Execute(context.Background(), call(`{"command":"git status"}`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1", calls)
		}
		if res.IsError || res.Content != "ok" {
			t.Fatalf("res = %+v, want delegated ok", res)
		}
		if proposed != "git status" {
			t.Fatalf("proposed command = %q, want %q", proposed, "git status")
		}
	})

	t.Run("rejected command does not delegate and is normal flow", func(t *testing.T) {
		calls := 0
		gate := commandApprovalGate{
			inner:   stubTool{name: "bash", calls: &calls},
			propose: stubPropose(false, "The developer rejected this command.", false, nil),
		}
		res := gate.Execute(context.Background(), call(`{"command":"git push"}`))
		if calls != 0 {
			t.Fatalf("inner Execute called %d times, want 0 (rejected)", calls)
		}
		if res.IsError {
			t.Fatal("developer rejection must be normal flow, not a tool error")
		}
		if !strings.Contains(res.Content, "rejected") {
			t.Fatalf("res.Content = %q, want rejection note", res.Content)
		}
	})

	t.Run("fatal proposal surfaces as tool error", func(t *testing.T) {
		gate := commandApprovalGate{
			inner:   stubTool{name: "bash"},
			propose: stubPropose(false, "Error: agent canceled", true, nil),
		}
		res := gate.Execute(context.Background(), call(`{"command":"ls"}`))
		if !res.IsError {
			t.Fatal("fatal proposal path must surface as a tool error")
		}
	})

	t.Run("unparseable args bypass proposal and delegate", func(t *testing.T) {
		calls := 0
		gate := commandApprovalGate{
			inner: stubTool{name: "bash", calls: &calls},
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				t.Fatal("propose must not run for unparseable args")
				return false, "", false
			},
		}
		gate.Execute(context.Background(), call(`{not json`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1 (forwarded)", calls)
		}
	})

	t.Run("empty command bypasses proposal and delegates", func(t *testing.T) {
		calls := 0
		gate := commandApprovalGate{
			inner: stubTool{name: "bash", calls: &calls},
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				t.Fatal("propose must not run for an empty command")
				return false, "", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"  "}`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1 (forwarded)", calls)
		}
	})
}

func TestCommandApprovalGate_AllowlistSkipsProposal(t *testing.T) {
	allow := cmdallow.Load(t.TempDir())
	if err := allow.Add("go test ./..."); err != nil {
		t.Fatal(err)
	}

	t.Run("allowlisted command runs without proposing", func(t *testing.T) {
		calls := 0
		gate := commandApprovalGate{
			inner: stubTool{name: "bash", calls: &calls, result: upagent.ToolResult{Content: "ok"}},
			allow: allow,
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				t.Fatal("propose must not run for an allowlisted command")
				return false, "", false
			},
		}
		res := gate.Execute(context.Background(), call(`{"command":"go test ./..."}`))
		if calls != 1 || res.IsError {
			t.Fatalf("allowlisted command not delegated: calls=%d res=%+v", calls, res)
		}
	})

	t.Run("unlisted command still proposes", func(t *testing.T) {
		proposed := false
		gate := commandApprovalGate{
			inner: stubTool{name: "bash"},
			allow: allow,
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				proposed = true
				return false, "rejected", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"git push"}`))
		if !proposed {
			t.Fatal("unlisted command bypassed the proposal")
		}
	})

	t.Run("nil allowlist proposes everything", func(t *testing.T) {
		proposed := false
		gate := commandApprovalGate{
			inner: stubTool{name: "bash"},
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				proposed = true
				return false, "rejected", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"ls"}`))
		if !proposed {
			t.Fatal("nil allowlist must propose every command")
		}
	})
}

func TestCommandApprovalGate_GuardClasses(t *testing.T) {
	t.Run("search-class forwards without proposing", func(t *testing.T) {
		calls := 0
		gate := commandApprovalGate{
			inner: stubTool{name: "bash", calls: &calls},
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				t.Fatal("search-class command must not be proposed — the tool hard-blocks it")
				return false, "", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"grep -r foo ."}`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1 (tool produces the redirect)", calls)
		}
	})

	t.Run("destructive class carries its reason into the proposal", func(t *testing.T) {
		var gotReason string
		gate := commandApprovalGate{
			inner: stubTool{name: "bash"},
			propose: func(_ context.Context, _, _, reason string) (bool, string, bool) {
				gotReason = reason
				return false, "rejected", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"git push origin main"}`))
		if gotReason != "destructive command" {
			t.Fatalf("proposal reason = %q, want %q", gotReason, "destructive command")
		}
	})

	t.Run("plain command proposes with empty reason", func(t *testing.T) {
		var gotReason string
		reasonSet := false
		gate := commandApprovalGate{
			inner: stubTool{name: "bash"},
			propose: func(_ context.Context, _, _, reason string) (bool, string, bool) {
				gotReason, reasonSet = reason, true
				return false, "rejected", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"go build ./..."}`))
		if !reasonSet || gotReason != "" {
			t.Fatalf("plain command reason = %q (set=%v), want empty", gotReason, reasonSet)
		}
	})

	t.Run("allowlisted destructive command auto-runs", func(t *testing.T) {
		// The developer explicitly always-allowed the exact command; the
		// allowlist decision outranks the guard class (the managed inner
		// tool no longer re-blocks it).
		allow := cmdallow.Load(t.TempDir())
		if err := allow.Add("git push origin main"); err != nil {
			t.Fatal(err)
		}
		calls := 0
		gate := commandApprovalGate{
			inner: stubTool{name: "bash", calls: &calls},
			allow: allow,
			propose: func(context.Context, string, string, string) (bool, string, bool) {
				t.Fatal("allowlisted command must not be proposed")
				return false, "", false
			},
		}
		gate.Execute(context.Background(), call(`{"command":"git push origin main"}`))
		if calls != 1 {
			t.Fatalf("inner Execute called %d times, want 1", calls)
		}
	})
}

func TestNew_ApproveBashCommands_BashIsApprovalManaged(t *testing.T) {
	// The wrapped bash tool must have its approval-eligible guard
	// classes relaxed, or an approved destructive command would be
	// re-blocked on execution. Verified via the prompt-guidance variant
	// (the managed tool describes approval instead of prohibition).
	ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{ApproveBashCommands: true})
	t.Cleanup(ag.Close)
	notes := kit.ToolPromptGuidelines(registeredBash(ag))
	if len(notes) != 1 || !strings.Contains(notes[0], "approval") {
		t.Fatalf("wrapped bash guidance = %v, want the approval-managed variant", notes)
	}

	plain := New(&multiTurnProvider{}, stubWorkspace{}, nil)
	t.Cleanup(plain.Close)
	plainNotes := kit.ToolPromptGuidelines(registeredBash(plain))
	if len(plainNotes) != 1 || strings.Contains(plainNotes[0], "approval") {
		t.Fatalf("unwrapped bash guidance = %v, want the default variant", plainNotes)
	}
}

// promptStubTool is a stubTool that also contributes prompt guidance,
// for wrapper-forwarding coverage.
type promptStubTool struct {
	stubTool
	notes []string
}

// PromptGuidelines satisfies kit.PromptContributor.
func (p promptStubTool) PromptGuidelines() []string { return p.notes }

func TestCommandApprovalGate_ForwardsPromptGuidelines(t *testing.T) {
	inner := promptStubTool{stubTool: stubTool{name: "bash"}, notes: []string{"bash guidance"}}
	gate := commandApprovalGate{inner: inner, propose: stubPropose(true, "", false, nil)}
	got := kit.ToolPromptGuidelines(gate)
	if len(got) != 1 || got[0] != "bash guidance" {
		t.Fatalf("PromptGuidelines = %v, want forwarded [bash guidance]; wrapper is stripping the optional interface", got)
	}
}

// registeredBash returns the agent's bash tool with the outer
// lifecycleAwareTool peeled off (bash is a mutating tool, so
// registration always adds that wrapper last).
func registeredBash(ag *Agent) Tool {
	t := ag.tools["bash"]
	if lt, ok := t.(lifecycleAwareTool); ok {
		return lt.Tool
	}
	return t
}

func TestNew_ApproveBashCommands_WrapsBash(t *testing.T) {
	t.Run("armed option wraps bash", func(t *testing.T) {
		ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{ApproveBashCommands: true})
		t.Cleanup(ag.Close)
		if _, ok := registeredBash(ag).(commandApprovalGate); !ok {
			t.Fatalf("bash tool is %T, want commandApprovalGate", registeredBash(ag))
		}
	})

	t.Run("default leaves bash unwrapped", func(t *testing.T) {
		ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
		t.Cleanup(ag.Close)
		if _, ok := registeredBash(ag).(commandApprovalGate); ok {
			t.Fatal("bash wrapped in commandApprovalGate without the option armed")
		}
	})

	t.Run("subagent grants win over approval", func(t *testing.T) {
		// A child agent must never block on an approval surface; the
		// grant gate is its only bash constraint even when the option
		// is (incorrectly) set.
		ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{
			ApproveBashCommands: true,
			BuiltinToolGrants:   mkMatcher(t, "bash", ""),
		})
		t.Cleanup(ag.Close)
		if _, ok := registeredBash(ag).(commandApprovalGate); ok {
			t.Fatal("granted (subagent) bash must not carry the approval gate")
		}
		if _, ok := registeredBash(ag).(bashGrantGate); !ok {
			t.Fatalf("granted bash is %T, want bashGrantGate", registeredBash(ag))
		}
	})
}

func TestProposeCommand(t *testing.T) {
	newAgentAndCoord := func(t *testing.T) (*Agent, *approval.Coordinator, context.Context) {
		t.Helper()
		ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
		t.Cleanup(ag.Close)
		coord := approval.New()
		return ag, coord, ctxWithCoord(context.Background(), coord)
	}

	t.Run("approve unblocks and permits", func(t *testing.T) {
		ag, coord, ctx := newAgentAndCoord(t)
		// Pre-queue the decision: the coordinator's cap-1 buffer absorbs
		// a signal sent before AwaitApproval parks (the headless race).
		coord.Approve("")
		approved, body, isError := ag.proposeCommand(ctx, "id-1", "git status", "")
		if !approved || body != "" || isError {
			t.Fatalf("proposeCommand = (%v, %q, %v), want approved", approved, body, isError)
		}
	})

	t.Run("reject returns rejection note as normal flow", func(t *testing.T) {
		ag, coord, ctx := newAgentAndCoord(t)
		coord.Reject()
		approved, body, isError := ag.proposeCommand(ctx, "id-2", "git push", "destructive command")
		if approved {
			t.Fatal("rejected command reported approved")
		}
		if isError {
			t.Fatal("developer rejection must be normal flow, not a tool error")
		}
		if !strings.Contains(body, "rejected") {
			t.Fatalf("body = %q, want rejection note", body)
		}
	})

	t.Run("missing coordinator is fatal", func(t *testing.T) {
		ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
		t.Cleanup(ag.Close)
		approved, body, isError := ag.proposeCommand(context.Background(), "id-3", "ls", "")
		if approved || !isError {
			t.Fatalf("proposeCommand without coord = (%v, %q, %v), want fatal", approved, body, isError)
		}
	})

	t.Run("cancellation is fatal", func(t *testing.T) {
		ag, _, ctx := newAgentAndCoord(t)
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		approved, _, isError := ag.proposeCommand(ctx, "id-4", "ls", "")
		if approved || !isError {
			t.Fatal("canceled proposal must be fatal, not approved")
		}
	})
}
