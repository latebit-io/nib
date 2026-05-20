package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/latebit-io/nib/tui/ui/theme"
	"github.com/mattn/go-runewidth"
)

// Beat / Block model for the agent pane.
//
// A Beat is one user → agent → result cycle (renamed from "turn" in the
// display layer; engine code still uses "turn" for LLM-API alignment).
// Each Beat owns an ordered list of Blocks that classify what was emitted
// during that cycle: the user's message, agent prose, tool calls, errors,
// proposals, etc.
//
// Step 0 of the agent-pane redesign introduces this structure *alongside*
// the existing RawLines + per-line classification maps (userRawLines,
// metaRawLines, turnSeparatorRawLines). The line buffer remains the text
// substrate; beats become the source of truth for *grouping and type* so
// later steps can:
//
//   - render typed left-border blocks (proposal/error/lint/smoke) by
//     walking beats rather than re-classifying lines (step 3),
//   - re-style completed beats through the dim palette without losing
//     per-block hue (step 5),
//   - drive per-block collapse/expand state independently from the line
//     buffer (step 6).
//
// Append paths populate beats *additively* — the existing classification
// maps stay correct so the current Render path continues to work
// unchanged until those later steps migrate it.

// blockBorderGlyph is the 2-cell colored left margin attached to
// proposal / error / lint / smoke blocks. A heavy left vertical block
// plus a one-cell pad — the pad gives breathing room between the
// border and the content while staying narrow enough to fit short
// proposals on a single rendered row.
const blockBorderGlyph = "▎ "

// blockBorderWidth is the rendered cell width of [blockBorderGlyph].
// Render paths use this to reduce the effective content width before
// padding / truncation so the prefixed border doesn't push content
// past the pane edge.
const blockBorderWidth = 2

// BeatStatus is the lifecycle state of a [Beat].
type BeatStatus uint8

const (
	// BeatRunning is the active beat — the agent may still emit blocks.
	BeatRunning BeatStatus = iota
	// BeatDone is a beat that finished without an explicit error.
	BeatDone
	// BeatFailed is a beat where an error block was emitted.
	BeatFailed
	// BeatInterrupted is a beat the developer interrupted (esc / Ctrl+C).
	BeatInterrupted
)

// BlockKind classifies a [Block]'s semantic role within its parent [Beat].
// Drives left-border color and collapse defaults in the redesign render
// path; for step 0 it's only metadata recorded as content flows in.
type BlockKind uint8

const (
	// BlockUserMessage is the developer's "You: …" message that opens a beat.
	BlockUserMessage BlockKind = iota
	// BlockAgentText is streaming agent prose (markdown).
	BlockAgentText
	// BlockToolCall is a "● tool" bullet emitted when the agent invokes a tool.
	BlockToolCall
	// BlockMeta is generic chrome (status updates, session summaries, "Done"
	// markers) that doesn't fit a more specific kind. Catch-all for the
	// current AppendMeta callsites that haven't been typed yet.
	BlockMeta
	// BlockProposal is an edit proposal ("--- Proposed: … ---").
	BlockProposal
	// BlockError is an error notice ("Error: …") emitted by the engine.
	BlockError
	// BlockTurnUsage is a standalone "◇ ↑X ↓Y" turn-cost line — emitted
	// when a turn ended with no tool call to fold the stats into.
	BlockTurnUsage
)

// blockOpen is the [Block.EndRaw] sentinel marking a block as still
// receiving content (e.g. an in-flight agent-text block during streaming).
// Closed when the next non-text block arrives or the beat itself closes.
const blockOpen = -1

// Block is one structural element inside a [Beat]. Currently indexes a
// span of raw lines in [AgentPaneModel.RawLines]; later steps will hang
// per-kind state here (tool status, progress, error refs) so the render
// path can walk beats directly instead of re-classifying lines.
type Block struct {
	// Kind is the semantic role of this block.
	Kind BlockKind
	// StartRaw is the inclusive raw-line index where the block starts.
	StartRaw int
	// EndRaw is the exclusive raw-line index where the block ends, or
	// [blockOpen] when the block is still open (currently only used for
	// streaming agent-text blocks).
	EndRaw int
	// Collapsed is true when the block is rendered in collapsed form.
	// Defaults vary by kind and beat status; populated in step 6.
	Collapsed bool
	// ToolName is set for [BlockToolCall]; empty for every other kind.
	ToolName string
}

// Beat groups the [Block]s of one user → agent exchange cycle.
type Beat struct {
	// Number is the 1-indexed beat counter; matches the visible "beat N"
	// label in the redesign render path. The implicit first beat created
	// at pane construction is Number=1.
	Number int
	// Status is the lifecycle state.
	Status BeatStatus
	// StartRaw is the raw-line index where the beat's content begins:
	// the separator line for beats ≥ 2, or 0 for the implicit first beat.
	StartRaw int
	// Blocks are the structural elements of this beat in emission order.
	Blocks []Block
}

// initBeats seeds the implicit first beat. Called by [NewAgentPaneModel]
// so beats is never nil and the engine bridge can always find a current
// beat to attach blocks to without nil-guards at every callsite.
func (m *AgentPaneModel) initBeats() {
	m.beats = []Beat{{Number: 1, Status: BeatRunning, StartRaw: 0}}
}

// currentBeat returns a pointer to the open beat. Panics if [initBeats]
// was never called — that would be a construction bug, not a runtime
// state to defend against.
func (m *AgentPaneModel) currentBeat() *Beat {
	return &m.beats[len(m.beats)-1]
}

// closeOpenBlockAt closes any open block on the current beat at the
// given raw index. The close-point matters: it must be the raw index
// of whatever content displaces the open block (i.e. the StartRaw of
// the new block being appended, or the StartRaw of a new beat). Closing
// at len(RawLines) would overlap with subsequent content the caller is
// in the middle of adding and break the non-overlapping range invariant
// blockAt relies on.
func (m *AgentPaneModel) closeOpenBlockAt(closeAt int) {
	b := m.currentBeat()
	if len(b.Blocks) == 0 {
		return
	}
	last := &b.Blocks[len(b.Blocks)-1]
	if last.EndRaw == blockOpen {
		last.EndRaw = closeAt
	}
}

// appendBlock closes any open block of a different kind and appends a
// new block with [startRaw, endRaw) on the current beat. endRaw may be
// [blockOpen] for streaming blocks. If the new block's kind matches an
// existing open block, the open block's range is extended instead so
// streaming runs stay merged.
//
// Non-overlapping ranges are an invariant: when a same-beat block of a
// different kind displaces an open block, the open block is closed at
// the new block's StartRaw so adjacent blocks abut without gap or
// overlap. [blockAt] returns the first range that covers a raw index,
// so any overlap would silently mis-classify the second block's lines.
func (m *AgentPaneModel) appendBlock(kind BlockKind, startRaw, endRaw int, toolName string) {
	b := m.currentBeat()

	// Extend an open block of the same kind rather than splitting — this
	// is the streaming-agent-text case where each AppendToken can land
	// across multiple raw lines but should remain one logical block.
	if len(b.Blocks) > 0 {
		last := &b.Blocks[len(b.Blocks)-1]
		if last.EndRaw == blockOpen && last.Kind == kind {
			if endRaw != blockOpen {
				last.EndRaw = endRaw
			}
			return
		}
	}
	m.closeOpenBlockAt(startRaw)
	b.Blocks = append(b.Blocks, Block{
		Kind:     kind,
		StartRaw: startRaw,
		EndRaw:   endRaw,
		ToolName: toolName,
	})
}

// blockAt returns the [Block] containing the given wrapped-line index
// along with the beat it belongs to. The ok result is false when the
// line falls between blocks (e.g. on a turn separator, a blank padding
// row AppendText emits, or any pre-block preamble) — callers should
// fall through to default per-line classification in that case.
//
// Search walks beats in reverse and blocks in order: the active beat
// is hit first and lookups within a beat are linear over its few
// blocks, so this is well under the per-frame budget at typical
// transcript sizes. If profiling shows otherwise, the right next step
// is a beat-indexed jump table built lazily on append, not premature
// caching here.
func (m *AgentPaneModel) blockAt(wrappedIdx int) (block Block, beat *Beat, ok bool) {
	rawIdx := m.rawIndexOf(wrappedIdx)
	if rawIdx < 0 {
		return Block{}, nil, false
	}
	for bi := len(m.beats) - 1; bi >= 0; bi-- {
		b := &m.beats[bi]
		if b.StartRaw > rawIdx {
			continue
		}
		for _, blk := range b.Blocks {
			end := blk.EndRaw
			if end == blockOpen {
				end = len(m.RawLines)
			}
			if rawIdx >= blk.StartRaw && rawIdx < end {
				return blk, b, true
			}
		}
		return Block{}, nil, false
	}
	return Block{}, nil, false
}

// hasLeftBorder reports whether a [BlockKind] renders with a colored
// left border in the redesigned pane. Centralized here so renderer and
// width-math callers agree on the answer.
func hasLeftBorder(kind BlockKind) bool {
	switch kind {
	case BlockProposal, BlockError:
		return true
	}
	return false
}

// blockBorderColor returns the bright-palette left-border color for a
// kind that has a border. The dim caller is responsible for routing
// the result through [theme.Dim] when the owning beat is past.
//
// Returns nil for kinds without a border — callers that pass an
// unbordered kind are misusing the helper and should be guarded by
// [hasLeftBorder] first.
func blockBorderColor(kind BlockKind) color.Color {
	switch kind {
	case BlockProposal:
		return theme.Proposal
	case BlockError:
		return theme.ErrorBorder
	}
	return nil
}

// blockBodyStyle returns the lipgloss style applied to the content of a
// bordered block. Proposal text reads at primary brightness so the
// reason is legible; error text takes the error-text hue so the body
// echoes the border color.
//
// The dim flag is honored here (not just on the border) so a past
// beat's bordered block fades both border and body together.
func blockBodyStyle(kind BlockKind, dim bool) lipgloss.Style {
	switch kind {
	case BlockProposal:
		fg := theme.PrimaryText
		if dim {
			fg = theme.PrimaryTextDim
		}
		return lipgloss.NewStyle().Foreground(fg)
	case BlockError:
		fg := theme.ErrorText
		if dim {
			fg = theme.ErrorTextDim
		}
		return lipgloss.NewStyle().Foreground(fg)
	}
	return lipgloss.NewStyle()
}

// renderBorderedLine produces a fully-decorated row for a wrapped line
// that lives inside a bordered block. The output is exactly m.width
// cells wide: a 2-cell colored left border followed by the styled
// content padded to width-2.
//
// The dim flag picks bright vs past-beat palette stops for both the
// border and the body — kept here in one place so a past beat's
// proposal still reads as a proposal (preserved hue), not as gray.
//
// Returns the input unchanged when the pane is too narrow to fit even
// the border — better to show clipped content than a half-rendered
// border at width < 3.
func (m *AgentPaneModel) renderBorderedLine(lineText string, kind BlockKind, dim bool) string {
	if m.width < blockBorderWidth+1 {
		return m.padLine(lineText)
	}
	contentW := m.width - blockBorderWidth

	bodyW := runewidth.StringWidth(lineText)
	var body string
	if bodyW >= contentW {
		body = runewidth.Truncate(lineText, contentW, "")
	} else {
		body = lineText + strings.Repeat(" ", contentW-bodyW)
	}
	body = blockBodyStyle(kind, dim).Render(body)

	borderC := blockBorderColor(kind)
	if dim {
		borderC = theme.Dim(borderC)
	}
	border := lipgloss.NewStyle().Foreground(borderC).Render(blockBorderGlyph)
	return border + body
}

// openBeat closes the previous beat (marking it Done unless already in a
// terminal state) and opens a new beat starting at the separator's raw
// line. Returns a pointer to the new beat.
//
// Called from [AppendUserMessage] after the separator has been appended
// to RawLines but before the user-message text — separatorRaw is the
// index of the "── beat N ──" placeholder so the beat owns the divider
// row alongside its blocks.
func (m *AgentPaneModel) openBeat(number, separatorRaw int) *Beat {
	if len(m.beats) > 0 {
		prev := &m.beats[len(m.beats)-1]
		if prev.Status == BeatRunning {
			prev.Status = BeatDone
		}
		// Close the trailing open block on the previous beat at the
		// separator raw so its range is finite for later render walks
		// and doesn't extend into the new beat's content.
		if len(prev.Blocks) > 0 {
			last := &prev.Blocks[len(prev.Blocks)-1]
			if last.EndRaw == blockOpen {
				last.EndRaw = separatorRaw
			}
		}
	}
	m.beats = append(m.beats, Beat{
		Number:   number,
		Status:   BeatRunning,
		StartRaw: separatorRaw,
	})
	return &m.beats[len(m.beats)-1]
}
