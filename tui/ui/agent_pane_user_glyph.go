package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/latebit-io/nib/tui/ui/theme"
	"github.com/mattn/go-runewidth"
)

// User-message glyph classification (step 7 of the agent-pane redesign).
//
// Replaces the static "You:" prefix with a small icon that reflects the
// content or submission path of the message. Three deterministic states
// — see [/nib/plans/agent-pane-redesign.md] for why heuristic states
// (approval / reject / question / plan / error) were cut from v1.

// UserGlyph picks the icon prefixed onto a user message.
type UserGlyph uint8

const (
	// UserGlyphNeutral is the default — no @-refs, not interrupt-sent.
	UserGlyphNeutral UserGlyph = iota
	// UserGlyphAttached marks messages that contain @<path> file refs.
	UserGlyphAttached
	// UserGlyphInterrupt marks messages sent via the interrupt path.
	// Not produced by [classifyUserMessage] — callers pass this kind
	// explicitly through [AgentPaneModel.AppendInterruptUserMessage].
	UserGlyphInterrupt
)

// userGlyphRune is the single rune rendered as the visible glyph for a
// kind. Width-1 in cell-count so the 2-cell "glyph + space" prefix
// added to user messages stays exactly 2 cells wide regardless of
// kind — predictable wrap math + alignment.
func userGlyphRune(g UserGlyph) string {
	switch g {
	case UserGlyphAttached:
		return "⎙"
	case UserGlyphInterrupt:
		return "✕"
	}
	return "◆"
}

// userGlyphPrefix is the literal "glyph + space" string inserted into
// the raw line buffer in place of the old "You: " prefix. Two cells
// wide for every kind.
func userGlyphPrefix(g UserGlyph) string {
	return userGlyphRune(g) + " "
}

// userGlyphPrefixCells is the cell-width of every [userGlyphPrefix]
// return value. Exposed as a constant so render-time width math
// doesn't have to call runewidth on each frame.
const userGlyphPrefixCells = 2

// Pre-built per-(kind, dim) glyph styles. Allocated once at package
// init so [renderUserLine] dispatches via a switch without touching
// lipgloss.NewStyle() per rendered frame — render-path allocations
// are an explicit project no-no.
var (
	userGlyphStyleAttachedBright  = lipgloss.NewStyle().Foreground(theme.ProposalText)
	userGlyphStyleAttachedDim     = lipgloss.NewStyle().Foreground(theme.ProposalTextDim)
	userGlyphStyleInterruptBright = lipgloss.NewStyle().Foreground(theme.Interrupt)
	userGlyphStyleInterruptDim    = lipgloss.NewStyle().Foreground(theme.InterruptDim)
)

// userGlyphStyle returns the pre-built lipgloss style for a glyph
// kind. The body of the user message keeps its uniform accent color —
// only the glyph cell differentiates kind (per the cut-bubble-bg-tint
// scope).
//
// The dim flag picks the hue-preserving past-beat counterpart so a
// past attached message still reads as "attached" rather than fading
// into the generic accent-dim.
//
// Neutral falls back to the user-body styles since the glyph hue
// matches the body — there's no per-frame visible difference between
// split and uniform rendering for that kind. The render path's
// fast-path short-circuit catches that case before this call.
func userGlyphStyle(g UserGlyph, dim bool) lipgloss.Style {
	switch g {
	case UserGlyphAttached:
		if dim {
			return userGlyphStyleAttachedDim
		}
		return userGlyphStyleAttachedBright
	case UserGlyphInterrupt:
		if dim {
			return userGlyphStyleInterruptDim
		}
		return userGlyphStyleInterruptBright
	}
	if dim {
		return userMessageDimStyle
	}
	return userMessageStyle
}

// classifyUserMessage picks a deterministic glyph for a freshly-submitted
// user message based on its text. Returns [UserGlyphAttached] when the
// message carries one or more @<path> tokens; otherwise [UserGlyphNeutral].
// [UserGlyphInterrupt] is supplied by the interrupt code path, not
// inferred here.
//
// Heuristic states (approval, reject, question, plan, error) are
// intentionally absent — the trimmed plan cut them because regex
// misfires (e.g. "go fix this" → approval) would make glyphs feel
// random rather than informative.
func classifyUserMessage(text string) UserGlyph {
	if hasFileRef(text) {
		return UserGlyphAttached
	}
	return UserGlyphNeutral
}

// renderUserLine produces the styled row for a wrapped line that's
// part of a user message. When the wrapped line is the FIRST wrapped
// row of a raw line carrying a glyph (recorded on its rawLineMark),
// the leading 2 cells render in the glyph's hue and the rest renders
// in the uniform user body color (Accent or AccentDim). Continuation
// wrapped rows and the legacy no-glyph path render uniform.
//
// Width math: the prefix is always exactly [userGlyphPrefixCells]
// (= 2) cells regardless of glyph kind because every glyph rune
// chosen here measures 1 cell and the trailing space is 1 cell. If a
// future glyph violates that assumption [runewidth.StringWidth] on
// the prefix would expose it and this split would shift content.
func (m *AgentPaneModel) renderUserLine(lineText string, wrappedIdx int, dim bool) string {
	bodyStyle := userMessageStyle
	if dim {
		bodyStyle = userMessageDimStyle
	}

	rawIdx := m.rawIndexOf(wrappedIdx)
	glyph := m.rawMarks[rawIdx].glyph
	firstWrapped := rawIdx >= 0 && rawIdx < len(m.wrappedIndex) && m.wrappedIndex[rawIdx] == wrappedIdx

	// Neutral (also the zero value on non-glyph lines) matches the body
	// color exactly — no visible difference between split and uniform
	// rendering, so skip the split work. Same for continuation rows.
	if !firstWrapped || glyph == UserGlyphNeutral {
		return bodyStyle.Render(m.padLine(lineText))
	}

	runes := []rune(lineText)
	if len(runes) < userGlyphPrefixCells {
		return bodyStyle.Render(m.padLine(lineText))
	}
	glyphPart := string(runes[:userGlyphPrefixCells])
	body := string(runes[userGlyphPrefixCells:])
	bodyW := runewidth.StringWidth(body)
	pad := m.width - userGlyphPrefixCells - bodyW
	if pad < 0 {
		body = runewidth.Truncate(body, m.width-userGlyphPrefixCells, "")
		pad = 0
	}
	body += strings.Repeat(" ", pad)

	return userGlyphStyle(glyph, dim).Render(glyphPart) + bodyStyle.Render(body)
}

// hasFileRef reports whether text contains an @<path> token. A token
// qualifies when it is whitespace-separated and begins with "@" followed
// by at least one non-space, non-"@" rune.
//
// Word-boundary check via [strings.Fields] keeps "user@example.com"
// from triggering (the @ is mid-token); the second-rune guard rejects
// "@@" and bare "@" so they don't false-positive either.
func hasFileRef(text string) bool {
	for _, tok := range strings.Fields(text) {
		if len(tok) >= 2 && tok[0] == '@' && tok[1] != '@' {
			return true
		}
	}
	return false
}
