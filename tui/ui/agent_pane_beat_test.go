package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/coding/event"
)

// beatPane builds a pane sized large enough that AppendText / AppendMeta
// have room to land their raw lines without scroll clamping interfering.
func beatPane() *AgentPaneModel {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	m.SetSize(80, 30)
	return m
}

// TestInitBeats_ImplicitFirstBeat verifies the pane starts with exactly
// one open beat numbered 1 — every callsite can rely on currentBeat()
// being non-nil without checking len(beats) first.
func TestInitBeats_ImplicitFirstBeat(t *testing.T) {
	m := beatPane()
	if got, want := len(m.beats), 1; got != want {
		t.Fatalf("beats count = %d; want %d", got, want)
	}
	b := m.beats[0]
	if b.Number != 1 {
		t.Errorf("beat 0 Number = %d; want 1", b.Number)
	}
	if b.Status != BeatRunning {
		t.Errorf("beat 0 Status = %d; want BeatRunning", b.Status)
	}
	if got := len(b.Blocks); got != 0 {
		t.Errorf("beat 0 Blocks count = %d; want 0", got)
	}
}

// TestAppendToken_OpensAndExtendsAgentTextBlock verifies a streaming
// burst lands as a single open BlockAgentText that extends across
// multiple AppendToken calls — repeated same-kind appends must merge,
// not split, so the rendered timeline shows one prose block per burst.
func TestAppendToken_OpensAndExtendsAgentTextBlock(t *testing.T) {
	m := beatPane()
	m.AppendToken("hello ")
	m.AppendToken("world\n")
	m.AppendToken("second line\n")

	if got := len(m.beats); got != 1 {
		t.Fatalf("beats count = %d; want 1", got)
	}
	blocks := m.beats[0].Blocks
	if got := len(blocks); got != 1 {
		t.Fatalf("blocks count = %d; want 1 (streaming burst should merge)", got)
	}
	if blocks[0].Kind != BlockAgentText {
		t.Errorf("block Kind = %d; want BlockAgentText", blocks[0].Kind)
	}
	if blocks[0].EndRaw != blockOpen {
		t.Errorf("block EndRaw = %d; want blockOpen sentinel (-1)", blocks[0].EndRaw)
	}
}

// TestAppendToolCall_SplitsAgentTextBlock verifies a tool call arriving
// mid-stream closes the open agent-text block and opens a typed
// BlockToolCall — a heterogeneous chunk must split the timeline so
// later render walks can border the tool call independently.
func TestAppendToolCall_SplitsAgentTextBlock(t *testing.T) {
	m := beatPane()
	m.AppendToken("planning the change\n")
	m.AppendToolCall("apply_patch")
	m.AppendToken("more prose after the call\n")

	blocks := m.beats[0].Blocks
	if got, want := len(blocks), 3; got != want {
		t.Fatalf("blocks count = %d; want %d", got, want)
	}
	wantKinds := []BlockKind{BlockAgentText, BlockToolCall, BlockAgentText}
	for i, want := range wantKinds {
		if blocks[i].Kind != want {
			t.Errorf("block[%d].Kind = %d; want %d", i, blocks[i].Kind, want)
		}
	}
	if blocks[1].ToolName != "apply_patch" {
		t.Errorf("tool block ToolName = %q; want %q", blocks[1].ToolName, "apply_patch")
	}
	// The first agent-text block should have a finite EndRaw now that
	// the tool call closed it.
	if blocks[0].EndRaw == blockOpen {
		t.Errorf("first agent-text block still open after tool call")
	}
}

// TestAppendUserMessage_OpensNewBeatAndClosesPrevious verifies the
// beat lifecycle: a new user message finalizes the prior beat (Done)
// and opens a fresh BeatRunning anchored at the separator with the
// user-message block as its first content.
func TestAppendUserMessage_OpensNewBeatAndClosesPrevious(t *testing.T) {
	m := beatPane()
	m.AppendToken("first reply\n")

	m.AppendUserMessage("follow-up question")

	if got, want := len(m.beats), 2; got != want {
		t.Fatalf("beats count = %d; want %d", got, want)
	}
	if got := m.beats[0].Status; got != BeatDone {
		t.Errorf("prior beat Status = %d; want BeatDone", got)
	}
	// Prior beat's open block should have been closed during the transition.
	prevBlocks := m.beats[0].Blocks
	if last := prevBlocks[len(prevBlocks)-1]; last.EndRaw == blockOpen {
		t.Errorf("prior beat's last block still open after new beat")
	}

	b := m.beats[1]
	if b.Number != 2 {
		t.Errorf("new beat Number = %d; want 2", b.Number)
	}
	if b.Status != BeatRunning {
		t.Errorf("new beat Status = %d; want BeatRunning", b.Status)
	}
	if got := len(b.Blocks); got != 1 {
		t.Fatalf("new beat Blocks count = %d; want 1 (user message)", got)
	}
	if b.Blocks[0].Kind != BlockUserMessage {
		t.Errorf("new beat first block Kind = %d; want BlockUserMessage", b.Blocks[0].Kind)
	}
}

// TestAppendProposal_RecordsBorderedBlockKind verifies the typed
// proposal entry point records BlockProposal — the kind survives to the
// renderer instead of being indistinguishable BlockMeta chrome. The
// raw-line content keeps the prefixed "Proposed: …" placeholder so
// the engine bridge's prior behavior (visible text in transcript) is
// preserved for non-redesign code paths.
func TestAppendProposal_RecordsBorderedBlockKind(t *testing.T) {
	m := beatPane()
	m.AppendToken("planning\n")
	m.AppendProposal("Fix nil direction path")

	blocks := m.beats[0].Blocks
	if got := blocks[len(blocks)-1].Kind; got != BlockProposal {
		t.Errorf("last block Kind = %d; want BlockProposal", got)
	}
	if !hasLeftBorder(BlockProposal) {
		t.Errorf("BlockProposal must report hasLeftBorder() = true")
	}
	// Placeholder text reaches the line buffer so legacy render paths
	// without border decoration still show the proposal text.
	transcript := strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "Proposed: Fix nil direction path") {
		t.Errorf("raw transcript missing proposal text: %q", transcript)
	}
}

// TestAppendError_RecordsBorderedBlockKind verifies AppendError lands
// as BlockError with the "Error: …" prefix in the line buffer. Same
// dual-channel contract as AppendProposal.
func TestAppendError_RecordsBorderedBlockKind(t *testing.T) {
	m := beatPane()
	m.AppendError("compile failed: undefined symbol")

	blocks := m.beats[0].Blocks
	if got := blocks[len(blocks)-1].Kind; got != BlockError {
		t.Errorf("last block Kind = %d; want BlockError", got)
	}
	if !hasLeftBorder(BlockError) {
		t.Errorf("BlockError must report hasLeftBorder() = true")
	}
	transcript := strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "Error: compile failed") {
		t.Errorf("raw transcript missing error text: %q", transcript)
	}
}

// TestBlockAt_ResolvesWrappedLineToBlock verifies the binary-search
// lookup correctly maps a wrapped line index inside a bordered block
// back to that block. Specifically guards against off-by-one at the
// boundary: the line index of the FIRST wrapped row of the block must
// resolve to the block, not to whatever preceded it.
func TestBlockAt_ResolvesWrappedLineToBlock(t *testing.T) {
	m := beatPane()
	m.AppendToken("prose\n")
	m.AppendError("something broke")

	// Find the wrapped line carrying the error text.
	var hit int
	found := false
	for i, line := range m.Lines {
		if strings.Contains(line, "Error: something broke") {
			hit = i
			found = true
			break
		}
	}
	if !found {
		t.Fatal("error text not found in wrapped lines")
	}
	blk, _, ok := m.blockAt(hit)
	if !ok {
		t.Fatalf("blockAt(%d) returned ok=false; want a hit", hit)
	}
	if blk.Kind != BlockError {
		t.Errorf("blockAt(%d).Kind = %d; want BlockError", hit, blk.Kind)
	}
}

// TestRender_PastUserMessage_DimsAccentNotGray verifies the hue-
// preserving dim path: a user message in a completed (past) beat
// renders with AccentDim (#6a3a4a) rather than the catch-all gray
// dim chrome. Guards against a regression where past user lines fade
// to PrimaryTextDim and lose the "this was YOU" hierarchy across the
// scroll-back.
func TestRender_PastUserMessage_DimsAccentNotGray(t *testing.T) {
	m := beatPane()
	// First beat: a user message. After AppendUserMessage(beat 2), this
	// becomes the PRIOR beat (turnStartRaw advances past it).
	m.AppendUserMessage("first question")
	m.AppendToken("agent reply\n")
	m.AppendUserMessage("second question")

	out := m.Render()
	// AccentDim is #6a3a4a = decimal 106;58;74. Lipgloss emits
	// 24-bit color as "38;2;R;G;Bm" in the TrueColor escape sequence.
	const accentDimSeq = "38;2;106;58;74"
	const accentBrightSeq = "38;2;232;121;160" // #e879a0
	if !strings.Contains(out, accentDimSeq) {
		t.Errorf("past user message missing AccentDim color sequence %q in output", accentDimSeq)
	}
	if !strings.Contains(out, accentBrightSeq) {
		t.Errorf("current user message missing Accent color sequence %q in output", accentBrightSeq)
	}
}

// TestOpenBeat_AutoCollapsesPriorBeat verifies the auto-collapse policy:
// when a new beat opens, the prior beat (which transitioned out of
// BeatRunning) gets Collapsed=true so the past beat will render as a
// 1-line summary in the projection layer. The fresh BeatRunning beat
// stays expanded.
func TestOpenBeat_AutoCollapsesPriorBeat(t *testing.T) {
	m := beatPane()
	m.AppendToken("first reply\n")
	if m.beats[0].Collapsed {
		t.Fatalf("active beat must not be collapsed pre-transition")
	}

	m.AppendUserMessage("follow-up")

	if !m.beats[0].Collapsed {
		t.Errorf("prior beat Collapsed = false; want true (auto-collapse on transition)")
	}
	if m.beats[1].Collapsed {
		t.Errorf("active beat Collapsed = true; want false (new beat stays expanded)")
	}
}

// TestOpenBeat_UserOverridePreservesExpand verifies that a user-touched
// beat (UserOverride=true) is not re-collapsed by the auto policy. This
// is the contract that lets "Tab to expand a past beat" stick across
// subsequent beat boundaries — the developer's explicit preference wins.
func TestOpenBeat_UserOverridePreservesExpand(t *testing.T) {
	m := beatPane()
	m.AppendToken("first reply\n")

	// Simulate the user toggling open the active beat, then a new beat
	// opening. The Collapsed=false state must survive.
	m.beats[0].Collapsed = false
	m.beats[0].UserOverride = true

	m.AppendUserMessage("second send")

	if m.beats[0].Collapsed {
		t.Errorf("user-overridden beat was auto-collapsed; UserOverride must block the policy")
	}
}

// TestOpenBeat_StampsTimestamps verifies the StartedAt / EndedAt
// bookkeeping the collapsed-beat summary depends on: the new beat
// records its StartedAt, and the prior beat receives EndedAt at the
// same moment.
func TestOpenBeat_StampsTimestamps(t *testing.T) {
	m := beatPane()
	clock := time.Unix(1_700_000_000, 0)
	m.nowFunc = func() time.Time { return clock }

	m.AppendToken("first reply\n")
	clock = clock.Add(42 * time.Second)
	m.AppendUserMessage("follow-up")

	prev := m.beats[0]
	if prev.EndedAt.IsZero() {
		t.Errorf("prior beat EndedAt is zero after transition")
	}
	if got := m.beats[1].StartedAt; !got.Equal(clock) {
		t.Errorf("new beat StartedAt = %v; want %v", got, clock)
	}
}

// TestAppendTurnUsage_AggregatesOntoActiveBeat verifies turn-usage
// events accumulate onto the current beat's TokensIn/Out/Cached
// counters. The summary line draws from these instead of re-parsing
// the rendered turn-usage row text.
func TestAppendTurnUsage_AggregatesOntoActiveBeat(t *testing.T) {
	m := beatPane()
	m.AppendToken("prose\n")

	m.AppendTurnUsage(event.AgentTurnUsage{PromptTokens: 100, CompletionTokens: 25, CachedTokens: 40})
	m.AppendTurnUsage(event.AgentTurnUsage{PromptTokens: 60, CompletionTokens: 15})

	b := m.beats[0]
	if got, want := b.TokensIn, 160; got != want {
		t.Errorf("TokensIn = %d; want %d", got, want)
	}
	if got, want := b.TokensOut, 40; got != want {
		t.Errorf("TokensOut = %d; want %d", got, want)
	}
	if got, want := b.TokensCached, 40; got != want {
		t.Errorf("TokensCached = %d; want %d", got, want)
	}
}

// TestFlushPendingTurnUsage_RecordsTurnUsageBlock verifies the
// standalone "◇ ↑X ↓Y" turn-cost flush lands as a BlockTurnUsage so
// the render path can style it distinctly from generic meta chrome
// (currently both render dim, but step 3 wants them separable).
func TestFlushPendingTurnUsage_RecordsTurnUsageBlock(t *testing.T) {
	m := beatPane()
	m.AppendToken("a reply\n")
	m.AppendTurnUsage(event.AgentTurnUsage{PromptTokens: 120, CompletionTokens: 30})
	m.FlushPendingTurnUsage()

	blocks := m.beats[0].Blocks
	// Last block should be the turn-usage line. Inner ordering: agent
	// text, then the flushed usage line.
	if got := blocks[len(blocks)-1].Kind; got != BlockTurnUsage {
		t.Errorf("last block Kind = %d; want BlockTurnUsage", got)
	}
}
