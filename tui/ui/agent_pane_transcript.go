package ui

import (
	"fmt"
	"strings"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/tui/sanitize"
)

// Streaming-text transcript pipeline for AgentPaneModel.
//
// AppendToken / AppendText / AppendMeta / AppendUserMessage feed lines
// into RawLines and the wrapped Lines view; recomputeCodeBlock + the
// supporting parseFenceLine / isCodeLine track which lines fall inside
// a code fence so they bypass markdown rendering.
//
// State (RawLines, Lines, wrappedIndex, rawFenceAfter, inCodeAfter,
// metaRawLines, plainRawLines, turnSeparatorRawLines, userRawLines,
// streamingStartRaw, sanitizer) lives on AgentPaneModel — these
// methods are the focused mutation surface that owns it.

// AppendToken sanitizes and appends streaming text from the agent.
// Uses the stateful sanitizer to handle escape sequences split across chunks.
// Counts sanitized bytes for real-time output token estimation.
// Anchors the streaming tint watermark at the first token of a burst so
// all lines produced during this burst render with the live style.
func (m *AgentPaneModel) AppendToken(text string) {
	if m.streamingStartRaw < 0 {
		// Capture the raw index the token will land on: AppendText's first
		// part is merged into the last existing raw line, so the burst's
		// first line is the current end of RawLines (or 0 when empty).
		start := len(m.RawLines) - 1
		if start < 0 {
			start = 0
		}
		m.streamingStartRaw = start
	}
	clean := m.sanitizer.Sanitize(text)
	m.usage.streamingChars += len(clean)
	tokenStart := len(m.RawLines) - 1
	if tokenStart < 0 {
		tokenStart = 0
	}
	m.AppendText(clean)
	// Open or extend the streaming agent-text block on the current beat.
	// Subsequent same-burst calls land on the same block (appendBlock
	// extends an open block of matching kind), so a streaming run stays
	// one logical block in the timeline.
	m.appendBlock(BlockAgentText, tokenStart, blockOpen, "")
}

// AppendTurnUsage stashes the per-turn usage so the inline stats
// chunk can fold into the next tool-call bullet (Option B layout).
// If a prior turn's usage is still stashed (the prior turn ended
// with no tools), flush it as a standalone line first so each
// turn's stats appear exactly once.
//
// Also calls [UpdateUsage] to advance the running totals — those
// drive the cumulative session-end summary regardless of how
// per-turn stats are rendered.
func (m *AgentPaneModel) AppendTurnUsage(u event.AgentTurnUsage) {
	m.FlushPendingTurnUsage()
	uCopy := u
	m.pendingTurnUsage = &uCopy
	m.UpdateUsage(u)
	// Aggregate onto the current beat so the collapsed-beat summary can
	// surface "↑X ↓Y" without re-parsing the rendered usage line. Done
	// here rather than in FlushPendingTurnUsage because some turns end
	// with a tool call that folds the stats inline (no flush) — both
	// paths still go through AppendTurnUsage first, so this is the one
	// place the cost lands per turn.
	m.addTurnTokens(u.PromptTokens, u.CompletionTokens, u.CachedTokens)
}

// FlushPendingTurnUsage renders any stashed turn usage as a
// standalone line (used when a turn ends with no tool to fold
// into, e.g. the final assistant reply or a turn that consisted
// only of streaming text). No-op when nothing is pending. Exported
// so the app-level [event.AgentDone] handler can flush before the
// session summary.
//
// The standalone format keeps the same `↑X ↓Y [R Z · N%⚡]` shape
// the bullet line carries — just on its own row at column 0 with
// a `◇` glyph as the visual anchor so it reads as turn chrome.
func (m *AgentPaneModel) FlushPendingTurnUsage() {
	if m.pendingTurnUsage == nil {
		return
	}
	stats := formatTurnStatsInline(*m.pendingTurnUsage)
	if stats != "" {
		// Drop the leading " · " separator; this is a standalone
		// line so the marker is the ◇ glyph, not the dot.
		m.appendTypedMeta(BlockTurnUsage, "\n◇"+stats+"\n", "")
	}
	m.pendingTurnUsage = nil
}

// AppendToolCall renders a tool-call bullet. If a turn-usage is
// pending (this is the FIRST tool of a fresh turn), folds the
// stats inline as `● tool · ↑X ↓Y [R Z · N%⚡]`; subsequent tool
// bullets in the same turn (parallel tool calls) render unadorned
// since the cost was already attributed to the first bullet.
func (m *AgentPaneModel) AppendToolCall(name string) {
	if m.pendingTurnUsage != nil {
		stats := formatTurnStatsInline(*m.pendingTurnUsage)
		m.pendingTurnUsage = nil
		m.appendTypedMeta(BlockToolCall, "\n  ● "+name+stats+"\n", name)
		return
	}
	m.appendTypedMeta(BlockToolCall, "\n  ● "+name+"\n", name)
}

// AppendSubagentActivity renders a spawned subagent's progress as an
// indented, nested [BlockSubagent] chunk so a child run reads as its own
// sub-pane: a "▸ subagent <name>" header on start/finish framing the
// child's tool bullets, which are indented one level deeper than the
// parent's own tool calls.
func (m *AgentPaneModel) AppendSubagentActivity(ev event.SubagentActivity) {
	var line string
	switch ev.Phase {
	case event.SubagentStarted:
		line = "\n  ▸ subagent " + ev.Name + "…\n"
	case event.SubagentTool:
		line = "\n      ↳ " + ev.Detail + "\n"
	case event.SubagentFinished:
		mark := "✓"
		if !ev.Success {
			mark = "✗"
		}
		line = "\n  ▸ subagent " + ev.Name + " " + mark + "\n"
	default:
		return
	}
	m.appendTypedMeta(BlockSubagent, line, "")
}

// AppendMeta sanitizes and appends non-stream chrome text (tool calls, edit
// markers, errors, bracketed status updates, awaiting-input block). Marks
// the resulting raw lines so Render can style them dim, separating chrome
// from the agent's prose. Always begins on a fresh raw line so meta
// content cannot merge into an in-flight streaming token line.
//
// Uses a one-shot sanitizer so it doesn't interfere with the streaming
// sanitizer state.
func (m *AgentPaneModel) AppendMeta(text string) {
	m.appendTypedMeta(BlockMeta, text, "")
}

// appendTypedMeta is the shared implementation behind [AppendMeta],
// [AppendToolCall], and the turn-usage flush path. It performs the same
// sanitize / newline-guard / raw-range-marking work AppendMeta has
// always done, and additionally records a typed [Block] on the current
// beat so the redesigned render path can classify the chunk without
// pattern-matching against the rendered text.
//
// The toolName argument is only meaningful for [BlockToolCall];
// callers should pass "" for other kinds.
func (m *AgentPaneModel) appendTypedMeta(kind BlockKind, text, toolName string) {
	var s sanitize.Sanitizer
	clean := s.Sanitize(text)
	if !strings.HasPrefix(clean, "\n") {
		clean = "\n" + clean
	}
	// Force a trailing newline too. Without this, a caller passing a
	// single-line meta string (no "\n" terminator) would leave the meta
	// content as the current last raw line. AppendText reuses the last
	// raw line for the first chunk of the next append, so the first
	// agent token after the meta block would be merged into that line
	// and inherit the meta mark — rendering the token dim and leaking
	// meta fence state into the stream.
	if !strings.HasSuffix(clean, "\n") {
		clean += "\n"
	}
	firstRaw := len(m.RawLines)
	m.AppendText(clean)
	endRaw := len(m.RawLines)
	// Exclude the trailing empty raw line — AppendText reuses it for the
	// next incoming chunk, so marking it would dim the first agent token
	// after this meta block.
	if endRaw > firstRaw && m.RawLines[endRaw-1] == "" {
		endRaw--
	}
	if m.metaRawLines == nil {
		m.metaRawLines = make(map[int]bool)
	}
	for i := firstRaw; i < endRaw; i++ {
		m.metaRawLines[i] = true
	}
	// AppendText already ran recomputeCodeBlock, but it saw these lines
	// as untagged so a stray fence in LLM-supplied reason/error text
	// may have flipped rawFenceAfter. Recompute from firstRaw now that
	// metaRawLines is populated — the skip branch resets fence state.
	m.recomputeCodeBlock(firstRaw)
	m.invalidateMdCache()

	// Record the typed block on the current beat. A heterogeneous
	// chunk like a tool call necessarily ends the open agent-text run,
	// so appendBlock closes that block before opening this one.
	m.appendBlock(kind, firstRaw, endRaw, toolName)
}

// AppendProposal records an edit proposal block. text is the human-
// readable reason ("Fix nil direction path in ghost house movement
// update"). The rendered text reads "Proposed: REASON" and is marked
// [BlockProposal] so the render path draws the proposal-hue left
// border — the colored border already identifies the block kind, so
// the historical "── … ──" en-dash bracketing is dropped as redundant
// chrome.
//
// Same sanitize / newline-guard / meta-marking pipeline as [AppendMeta];
// only the recorded block kind differs. Engine bridges that previously
// hand-built the "--- Proposed: …" string and called AppendMeta should
// migrate to this entry point so the kind survives to the renderer.
func (m *AgentPaneModel) AppendProposal(reason string) {
	m.appendTypedMeta(BlockProposal, "\nProposed: "+reason+"\n", "")
}

// AppendError records an error block with [BlockError] kind so the
// render path draws the error-hue left border. text is the error
// message body; callers should pass the raw message without any
// "Error: " prefix — this method prepends the prefix.
//
// Same sanitize / newline-guard / meta-marking pipeline as [AppendMeta].
func (m *AgentPaneModel) AppendError(text string) {
	m.appendTypedMeta(BlockError, "\nError: "+text+"\n", "")
}

// QueueUserMessage stashes a developer submission that arrived while
// the agent was mid-stream. The Render() loop shows a transient
// "[queued: …]" banner row until [FlushPendingUserMessage] runs.
//
// Sanitized once on entry so the banner preview and the eventual
// committed "You: …" line share the same cleaned text.
func (m *AgentPaneModel) QueueUserMessage(text string) {
	var s sanitize.Sanitizer
	m.pendingUserMessage = s.Sanitize(text)
}

// HasPendingUserMessage reports whether a queued submission is awaiting
// the next turn boundary.
func (m *AgentPaneModel) HasPendingUserMessage() bool {
	return m.pendingUserMessage != ""
}

// FlushPendingUserMessage commits any queued user submission to the
// transcript via AppendUserMessage and clears the banner. No-op when
// nothing is pending. Called by the engine-event bridge on the first
// tool-less AgentTurnUsage after submission (the natural park boundary
// where the foundation would otherwise pick up the queued input), and
// defensively on AgentWaiting / AgentDone.
func (m *AgentPaneModel) FlushPendingUserMessage() {
	if m.pendingUserMessage == "" {
		return
	}
	text := m.pendingUserMessage
	m.pendingUserMessage = ""
	m.AppendUserMessage(text)
}

// AppendUserMessage classifies the developer's submission, picks a glyph
// (see [classifyUserMessage]), then appends through the shared
// [appendUserMessage] helper. Neutral / Attached are derived from the
// text; interrupt-path submissions should go through
// [AppendInterruptUserMessage] instead.
func (m *AgentPaneModel) AppendUserMessage(text string) {
	m.appendUserMessage(text, classifyUserMessage(text))
}

// AppendInterruptUserMessage records a user submission delivered through
// the interrupt code path. The glyph is always [UserGlyphInterrupt] —
// classification of text content is skipped because the path is what
// makes this kind deterministic.
func (m *AgentPaneModel) AppendInterruptUserMessage(text string) {
	m.appendUserMessage(text, UserGlyphInterrupt)
}

// appendUserMessage is the shared implementation behind the public user-
// message entry points. Marks the raw lines so Render() can style them
// distinctly, prepends the glyph + space prefix in place of the legacy
// "You: " label, and records the glyph kind on the prefixed raw line
// so the renderer can color the glyph cell separately from the body.
//
// Also stamps a "── ♩ beat N ──" divider and advances the dim watermark
// so the previous exchange fades into the background.
func (m *AgentPaneModel) appendUserMessage(text string, glyph UserGlyph) {
	var s sanitize.Sanitizer
	text = s.Sanitize(text)

	// Watermark for the dim split: everything with a raw index below this
	// is rendered dim. Captured before any append so the separator itself
	// belongs to the new turn (bright), and previous content fades.
	turnStart := len(m.RawLines)
	m.turnCounter++
	label := fmt.Sprintf("♩ beat %d", m.turnCounter)
	// Emit a compact placeholder — Render substitutes the full-width rule
	// using the label from turnSeparatorRawLines. Storing the label (not
	// parsing the rendered text) keeps the raw content small and stable
	// across resizes.
	m.AppendText("\n\n── " + label + " ──")
	sepRaw := len(m.RawLines) - 1
	if m.turnSeparatorRawLines == nil {
		m.turnSeparatorRawLines = make(map[int]string)
	}
	m.turnSeparatorRawLines[sepRaw] = label

	// Close the prior beat (Done unless already terminal) and open a new
	// one anchored at the separator. The user-message block on the new
	// beat is opened below after AppendText records its raw range.
	m.openBeat(m.turnCounter, sepRaw)

	// Second AppendText for the actual user text. Tracks its own start
	// index so the separator lines are NOT marked as user content.
	// The glyph + space prefix replaces the legacy "You: " label —
	// the prefix is part of the raw text so wrapping math sees it,
	// while userGlyphForRaw records the kind so the renderer can
	// color the glyph cell in its own hue.
	userStart := len(m.RawLines)
	prefix := userGlyphPrefix(glyph)
	m.AppendText("\n\n" + prefix + text + "\n\n")
	// Exclude the trailing empty raw line — AppendText reuses the last
	// raw line for the first chunk of the next append, so marking it
	// would misclassify the first agent token as a user message.
	if m.userRawLines == nil {
		m.userRawLines = make(map[int]bool)
	}
	endRaw := len(m.RawLines)
	if endRaw > userStart && m.RawLines[endRaw-1] == "" {
		endRaw--
	}
	for i := userStart; i < endRaw; i++ {
		m.userRawLines[i] = true
	}

	// AppendText ran recomputeCodeBlock before user lines were marked, so
	// fence state may have advanced through user content (e.g. an unmatched
	// "```" in the message). Recompute from the turn start now that
	// userRawLines is populated — this skips user lines and resets fence
	// state correctly.
	m.recomputeCodeBlock(turnStart)
	m.turnStartRaw = turnStart
	m.invalidateMdCache()

	// Record the user message as the opening block of the new beat. The
	// range is the same userStart..endRaw span the classification map
	// already covers — beats track the same content, just grouped.
	m.appendBlock(BlockUserMessage, userStart, endRaw, "")

	// Stamp the glyph kind on the raw-line that carries the prefix —
	// the first non-empty raw line in the appended range. AppendText
	// emits leading/trailing empty raws around the content; only the
	// content line carries the visible prefix and needs the glyph
	// override at render time.
	if m.userGlyphForRaw == nil {
		m.userGlyphForRaw = make(map[int]UserGlyph)
	}
	for i := userStart; i < endRaw; i++ {
		if m.RawLines[i] != "" {
			m.userGlyphForRaw[i] = glyph
			break
		}
	}
}

// recomputeCodeBlock rebuilds inCodeAfter starting from raw line index fromRaw.
// Fence detection runs on RawLines (logical lines) so that wrapping can never
// split or fabricate a fence. The per-raw-line state is then projected to all
// wrapped lines belonging to that raw line via wrappedIndex.
//
// Fence state is persisted in rawFenceAfter so incremental appends seed from
// rawFenceAfter[fromRaw-1] in O(1) instead of rescanning the entire prefix.
//
// Fences follow CommonMark rules: 3+ backticks or tildes, 0–3 leading spaces,
// opener can have info string, closer must use the same char at >= opener
// length with no non-space content after. This correctly handles nested fences
// (e.g. ```“ wrapping an inner ```).
func (m *AgentPaneModel) recomputeCodeBlock(fromRaw int) {
	// Size inCodeAfter to match Lines.
	for len(m.inCodeAfter) < len(m.Lines) {
		m.inCodeAfter = append(m.inCodeAfter, false)
	}
	m.inCodeAfter = m.inCodeAfter[:len(m.Lines)]

	// Truncate rawFenceAfter to fromRaw so we rebuild from there.
	if fromRaw < len(m.rawFenceAfter) {
		m.rawFenceAfter = m.rawFenceAfter[:fromRaw]
	}

	// Seed from persisted state — O(1).
	var fence fenceState
	if fromRaw > 0 && fromRaw-1 < len(m.rawFenceAfter) {
		fence = m.rawFenceAfter[fromRaw-1]
	}

	for ri := fromRaw; ri < len(m.RawLines); ri++ {
		// User messages are rendered with userMessageStyle, not markdown.
		// Skip them so an unmatched fence in user input doesn't bleed into
		// subsequent agent output.
		if m.userRawLines[ri] || m.plainRawLines[ri] || m.metaRawLines[ri] {
			// All three classes bypass fence detection: user messages,
			// awaiting-input blocks, and meta chrome (tool calls, edit
			// proposals, errors) may carry LLM-supplied text with
			// unmatched backticks that must not flip the state of
			// subsequent agent output.
			m.rawFenceAfter = append(m.rawFenceAfter, fence)
			wStart := m.wrappedIndex[ri]
			wEnd := len(m.Lines)
			if ri+1 < len(m.wrappedIndex) {
				wEnd = m.wrappedIndex[ri+1]
			}
			for wi := wStart; wi < wEnd; wi++ {
				m.inCodeAfter[wi] = false
			}
			continue
		}

		fenceBefore := fence
		fc, fl, closeable := parseFenceLine(m.RawLines[ri])
		if fl > 0 {
			if fence.len == 0 {
				// Not in a code block — any fence opens one (info string allowed).
				fence = fenceState{char: fc, len: fl}
			} else if closeable && fc == fence.char && fl >= fence.len {
				// In a code block — only close if no trailing non-space content.
				fence = fenceState{}
			}
		}

		// A raw line is "code" if we were inside a fence before processing it
		// (body + closer) OR if processing it opened a fence (opener). This
		// ensures all wrapped segments of a closer line are marked as code,
		// not just the first one.
		lineIsCode := fenceBefore.len > 0 || fence.len > 0

		// Persist fence state for this raw line.
		m.rawFenceAfter = append(m.rawFenceAfter, fence)

		// Fill all wrapped lines that belong to this raw line.
		wStart := m.wrappedIndex[ri]
		wEnd := len(m.Lines)
		if ri+1 < len(m.wrappedIndex) {
			wEnd = m.wrappedIndex[ri+1]
		}
		for wi := wStart; wi < wEnd; wi++ {
			m.inCodeAfter[wi] = lineIsCode
		}
	}
}

// parseFenceLine checks if line is a code fence (opener or closer).
// Returns the fence character ('`' or '~'), the run length, and whether the
// line can act as a closer (no non-space content after the fence run).
// Returns 0, 0, false if the line is not a fence at all.
//
// CommonMark rules: 0–3 leading spaces, 3+ of the same fence char. An opener
// may have trailing info text (canClose=false). A closer must have only
// optional trailing spaces (canClose=true).
func parseFenceLine(line string) (ch rune, count int, canClose bool) {
	runes := []rune(line)
	i := 0

	// Skip 0–3 leading spaces.
	spaces := 0
	for i < len(runes) && runes[i] == ' ' && spaces < 3 {
		i++
		spaces++
	}
	if i >= len(runes) {
		return 0, 0, false
	}

	ch = runes[i]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}

	// Count consecutive fence chars.
	start := i
	for i < len(runes) && runes[i] == ch {
		i++
	}
	count = i - start
	if count < 3 {
		return 0, 0, false
	}

	// A closer requires only optional trailing spaces after the fence run.
	canClose = true
	for j := i; j < len(runes); j++ {
		if runes[j] != ' ' && runes[j] != '\t' {
			canClose = false
			break
		}
	}

	return ch, count, canClose
}

// isCodeLine returns true when Lines[i] should be rendered with code block styling.
// This covers the opening fence, body lines, and the closing fence — all wrapped
// segments of a raw line share the same flag.
func (m *AgentPaneModel) isCodeLine(i int) bool {
	if i < 0 || i >= len(m.inCodeAfter) {
		return false
	}
	return m.inCodeAfter[i]
}
