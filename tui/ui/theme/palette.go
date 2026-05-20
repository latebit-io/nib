// Package theme is the single source of truth for the agent-pane redesign
// palette. Every color used by agent-pane rendering must come from this
// package — no inline lipgloss.Color("…") at callsites.
//
// The palette has three layers:
//
//   - Active: the bright stops, applied to the current beat.
//   - Dim: the same hue family at low brightness, applied to past beats.
//     Dimming is hue-preserving — never gray, never via lipgloss.Faint —
//     so a completed beat still tells you whether it errored, lint-passed,
//     or proposed a change at a glance.
//   - Surface: pane / bubble / divider backgrounds.
//
// Pairing is explicit: [Dim] maps each bright stop to its dim counterpart
// so render code can dim a styled element without knowing which hue
// family it belongs to.
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Active palette — current beat, full brightness.
var (
	// Accent is the nib pink. Active beat header, title, send button.
	Accent = lipgloss.Color("#e879a0")
	// Proposal is the left-border color for proposal blocks.
	Proposal = lipgloss.Color("#7F77DD")
	// ProposalText is the lighter accent for the "proposed" label
	// inside a proposal block.
	ProposalText = lipgloss.Color("#AFA9EC")
	// Success marks completed checkmarks and pass states.
	Success = lipgloss.Color("#5DCAA5")
	// SuccessBar is the filled section of a completed progress bar.
	SuccessBar = lipgloss.Color("#1D9E75")
	// ErrorBorder is the left-border color for error blocks.
	ErrorBorder = lipgloss.Color("#E24B4A")
	// ErrorText is the error message body color.
	ErrorText = lipgloss.Color("#F09595")
	// Warning is the running spinner / revert glyph.
	Warning = lipgloss.Color("#EF9F27")
	// WarningBar is the filled section of an in-progress bar.
	WarningBar = lipgloss.Color("#BA7517")
	// Info is the smoke-run / question-glyph hue.
	Info = lipgloss.Color("#85B7EB")
	// InfoBorder is the left-border for smoke-run blocks.
	InfoBorder = lipgloss.Color("#185FA5")
	// LintPass is the left-border for lint-pass blocks.
	LintPass = lipgloss.Color("#3B6D11")
	// PrimaryText is the default agent-prose / message-body color.
	PrimaryText = lipgloss.Color("#ccc")
	// SecondaryText is the tool-name / label color.
	SecondaryText = lipgloss.Color("#888")
	// MutedText is the token-counter / hint color.
	MutedText = lipgloss.Color("#444")
	// Interrupt is the user-glyph hue for messages sent via the
	// interrupt path. A warm orange chosen to read as "stop / cancel"
	// without overlapping the warning/error hue families.
	Interrupt = lipgloss.Color("#D85A30")
)

// Dim palette — past beats. Each color is the hue-preserving dim
// counterpart of its Active equivalent. Resolved via [Dim] when a
// render path wants to dim a styled element generically; named
// constants here so direct references at fixed-style sites stay
// readable.
var (
	// AccentDim pairs with [Accent].
	AccentDim = lipgloss.Color("#6a3a4a")
	// ProposalDim pairs with [Proposal].
	ProposalDim = lipgloss.Color("#3C3489")
	// ProposalTextDim pairs with [ProposalText].
	ProposalTextDim = lipgloss.Color("#534AB7")
	// SuccessDim pairs with [Success] (and with [SuccessBar] — completed
	// beats use a single dim stop for both border and filled progress).
	SuccessDim = lipgloss.Color("#0F6E56")
	// ErrorBorderDim pairs with [ErrorBorder].
	ErrorBorderDim = lipgloss.Color("#A32D2D")
	// ErrorTextDim pairs with [ErrorText].
	ErrorTextDim = lipgloss.Color("#791F1F")
	// WarningDim pairs with [Warning] (and [WarningBar]).
	WarningDim = lipgloss.Color("#854F0B")
	// InfoDim pairs with [Info] (and [InfoBorder] — single dim stop).
	InfoDim = lipgloss.Color("#0C447C")
	// LintPassDim pairs with [LintPass].
	LintPassDim = lipgloss.Color("#27500A")
	// PrimaryTextDim pairs with [PrimaryText].
	PrimaryTextDim = lipgloss.Color("#555")
	// SecondaryTextDim pairs with [SecondaryText]. Same hex as
	// [MutedText] — the dim secondary and active muted converge by
	// design (both read as "background information").
	SecondaryTextDim = lipgloss.Color("#444")
	// MutedTextDim pairs with [MutedText].
	MutedTextDim = lipgloss.Color("#2a2a2a")
	// InterruptDim pairs with [Interrupt]. Hue preserved at lower
	// brightness so a past interrupt glyph still reads as "stop"
	// rather than fading into generic warning gray.
	InterruptDim = lipgloss.Color("#6c2d18")
)

// Surface palette — pane / bubble / divider backgrounds. Surfaces don't
// dim: the same chrome holds bright and past-beat content alike.
var (
	// PaneBg is the agent pane background.
	PaneBg = lipgloss.Color("#0a0a0a")
	// TitleBar is the top-of-pane title bar background.
	TitleBar = lipgloss.Color("#111")
	// UserBubbleBg is the user-message bubble fill.
	UserBubbleBg = lipgloss.Color("#161616")
	// UserBubbleBorder is the user-message bubble border.
	UserBubbleBorder = lipgloss.Color("#2a2a2a")
	// ErrorBlockBg is the faint red wash inside an error block.
	ErrorBlockBg = lipgloss.Color("#1a0808")
	// LockedInputBg is the input box background while the agent is running.
	LockedInputBg = lipgloss.Color("#0d0d0d")
	// DividerActive is the active-beat hairline divider color.
	DividerActive = lipgloss.Color("#333")
	// DividerDim is the past-beat hairline divider color.
	DividerDim = lipgloss.Color("#1a1a1a")
)

// Dim returns the past-beat counterpart of a bright palette color. If c
// is not a known bright stop (e.g. an already-dim color, a surface, or
// an inline color a caller forgot to migrate), Dim returns c unchanged.
// That fallback keeps the renderer defensive without masking misuse —
// the result is "no visible change," which is easy to spot.
//
// Hue is preserved by construction: every pairing is the same family at
// lower brightness, never gray, so structural information (this beat
// errored / this proposal landed) survives the transition.
//
// Implementation is a switch rather than a map because lipgloss colors
// are returned as the [color.Color] interface — map-key equality on an
// interface depends on the dynamic type being comparable, and a switch
// keeps the pairing readable as a literal lookup table.
//
// Surfaces (pane bg, bubble bg, dividers) are intentionally absent:
// surfaces don't dim — the same chrome holds bright and past-beat
// content alike.
func Dim(c color.Color) color.Color {
	switch c {
	case Accent:
		return AccentDim
	case Proposal:
		return ProposalDim
	case ProposalText:
		return ProposalTextDim
	case Success, SuccessBar:
		return SuccessDim
	case ErrorBorder:
		return ErrorBorderDim
	case ErrorText:
		return ErrorTextDim
	case Warning, WarningBar:
		return WarningDim
	case Info, InfoBorder:
		return InfoDim
	case LintPass:
		return LintPassDim
	case PrimaryText:
		return PrimaryTextDim
	case SecondaryText:
		return SecondaryTextDim
	case MutedText:
		return MutedTextDim
	case Interrupt:
		return InterruptDim
	}
	return c
}
