package theme

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestDim_MapsKnownBrightStopsToDimCounterparts verifies the named
// pairings hold and that every Active stop has a Dim entry. New colors
// added to the palette must be reflected here so the dim path doesn't
// silently fall through to the identity branch.
func TestDim_MapsKnownBrightStopsToDimCounterparts(t *testing.T) {
	cases := []struct {
		name   string
		bright color.Color
		want   color.Color
	}{
		{"Accent", Accent, AccentDim},
		{"Proposal", Proposal, ProposalDim},
		{"ProposalText", ProposalText, ProposalTextDim},
		{"Success", Success, SuccessDim},
		{"SuccessBar", SuccessBar, SuccessDim},
		{"ErrorBorder", ErrorBorder, ErrorBorderDim},
		{"ErrorText", ErrorText, ErrorTextDim},
		{"Warning", Warning, WarningDim},
		{"WarningBar", WarningBar, WarningDim},
		{"Info", Info, InfoDim},
		{"InfoBorder", InfoBorder, InfoDim},
		{"LintPass", LintPass, LintPassDim},
		{"PrimaryText", PrimaryText, PrimaryTextDim},
		{"SecondaryText", SecondaryText, SecondaryTextDim},
		{"MutedText", MutedText, MutedTextDim},
		{"Interrupt", Interrupt, InterruptDim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Dim(tc.bright); got != tc.want {
				t.Errorf("Dim(%s) = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestDim_IdentityForUnknownColor verifies the defensive fallback: a
// color not in the dim map returns unchanged. Catches a renderer that
// holds an already-dim color or an inline hex that wasn't migrated.
func TestDim_IdentityForUnknownColor(t *testing.T) {
	unknown := lipgloss.Color("#123456")
	if got := Dim(unknown); got != unknown {
		t.Errorf("Dim(unknown) = %q; want input unchanged %q", got, unknown)
	}
	// Surfaces are intentionally not in the dim map either.
	if got := Dim(PaneBg); got != PaneBg {
		t.Errorf("Dim(PaneBg) = %q; want PaneBg unchanged (surfaces don't dim)", got)
	}
}
