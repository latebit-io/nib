package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/budget"
	kitcmd "github.com/latebit-io/nib/kit/command"
)

// ContextSnapshot mirrors [github.com/latebit-io/nib/coding/agent.ContextSnapshot]
// at the command boundary. Defined here (rather than imported) so this
// package stays independent of coding/agent — the cmd-level adapter
// converts between the two via direct field copy. Tests construct
// values directly without pulling in the full agent.
type ContextSnapshot struct {
	// Estimate is the per-section token estimate for the next request.
	Estimate llm.InputEstimate

	// ToolCount is the number of tool definitions the LLM will see.
	ToolCount int

	// MessageCount is the number of messages saved in the transcript.
	MessageCount int
}

// ContextSnapshotter is the minimum surface ContextCommand needs to
// produce its breakdown. *coding/agent.Agent satisfies the agent-side
// counterpart; the cmd-level adapter converts to this contract.
type ContextSnapshotter interface {
	// EstimateContext returns the per-section breakdown of the agent's
	// next request — no LLM call required.
	EstimateContext() ContextSnapshot

	// Usage returns the cumulative provider-reported usage for the run.
	Usage() budget.Session
}

// ContextCommand is the /context handler. It prints a per-section
// breakdown of the next request's estimated token cost plus the
// session's cumulative provider-reported usage. The intent is
// diagnostic: tell the developer which surface — system prompt, tool
// definitions, history — is dominating context consumption so the
// optimization decision (slim a prompt, trim a tool, compact) has a
// concrete target.
type ContextCommand struct {
	def       kitcmd.Definition
	snapshot  ContextSnapshotter
}

// NewContext returns a ContextCommand. snapshot must be non-nil; a nil
// snapshotter would silently render zeros and hide the misconfiguration
// from the user, so the constructor panics at registry-build time.
func NewContext(snapshot ContextSnapshotter) *ContextCommand {
	if snapshot == nil {
		panic("coding/command: NewContext requires a non-nil snapshotter")
	}
	return &ContextCommand{
		def: kitcmd.Definition{
			Name:        "context",
			Description: "Show a token breakdown for the next request and the session totals.",
			Source: kitcmd.Source{
				Kind: kitcmd.SourceBuiltin,
				Path: "coding/command",
			},
		},
		snapshot: snapshot,
	}
}

// Definition exposes the command's surface for /help and registry
// diagnostics.
func (c *ContextCommand) Definition() kitcmd.Definition { return c.def }

// Handle renders the breakdown into the transcript via Session.Display.
// Never returns an error — the snapshotter is pure-read and cannot fail.
func (c *ContextCommand) Handle(_ context.Context, sess kitcmd.Session, _ string) error {
	snap := c.snapshot.EstimateContext()
	usage := c.snapshot.Usage()
	sess.Display(formatContext(snap, usage))
	return nil
}

// formatContext renders the multi-line breakdown. Exported only to the
// package so tests can pin the output shape.
func formatContext(snap ContextSnapshot, usage budget.Session) string {
	est := snap.Estimate
	var sb strings.Builder

	sb.WriteString("Next request (estimated):\n")
	fmt.Fprintf(&sb, "  System prompt   %s tokens\n", fmtThousands(est.System))
	fmt.Fprintf(&sb, "  Tool defs       %s tokens  (%d tools)\n", fmtThousands(est.Tools), snap.ToolCount)
	fmt.Fprintf(&sb, "  History         %s tokens  (%d messages)\n", fmtThousands(est.History), snap.MessageCount)
	if est.New > 0 {
		fmt.Fprintf(&sb, "  New (pending)   %s tokens\n", fmtThousands(est.New))
	}
	sb.WriteString("  ───────────────────────────\n")
	fmt.Fprintf(&sb, "  Total           %s tokens\n", fmtThousands(est.Total))
	sb.WriteString("\n")

	if usage.Turns == 0 {
		sb.WriteString("Session totals: no turns yet")
		return sb.String()
	}

	fmt.Fprintf(&sb, "Session totals (%d turn(s)):\n", usage.Turns)
	fmt.Fprintf(&sb, "  Prompt          %s\n", fmtThousands(usage.TotalPromptTokens))
	fmt.Fprintf(&sb, "  Completion      %s\n", fmtThousands(usage.TotalCompletionTokens))
	cachedPct := 0
	if usage.TotalPromptTokens > 0 {
		cachedPct = (usage.TotalCachedTokens * 100) / usage.TotalPromptTokens
	}
	fmt.Fprintf(&sb, "  Cached          %s  (%d%% of prompt)\n", fmtThousands(usage.TotalCachedTokens), cachedPct)
	return sb.String()
}

// fmtThousands renders n with comma separators (e.g. 31822 → "31,822").
// Plain strconv + a single pass — no external dependency for a
// strictly-cosmetic concern.
func fmtThousands(n int) string {
	if n < 0 {
		return "-" + fmtThousands(-n)
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var sb strings.Builder
	first := len(s) % 3
	if first > 0 {
		sb.WriteString(s[:first])
		if len(s) > first {
			sb.WriteByte(',')
		}
	}
	for i := first; i < len(s); i += 3 {
		sb.WriteString(s[i : i+3])
		if i+3 < len(s) {
			sb.WriteByte(',')
		}
	}
	return sb.String()
}

// Compile-time check that ContextCommand satisfies HandlerCommand.
var _ kitcmd.HandlerCommand = (*ContextCommand)(nil)
