package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/tui/ui/theme"
)

// TestInputStateHintLine_PrecedenceOrder verifies the four-state hint
// line picks running → focused → empty-placeholder → legacy slash-hint
// in that order. The precedence matters: an animating agent must win
// over focus state, because the user looking at the pane needs to see
// "agent is busy / esc interrupt" rather than the generic send hint.
func TestInputStateHintLine_PrecedenceOrder(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*AgentPaneModel)
		wantSubs string // substring the hint must contain
	}{
		{
			"running wins over focused",
			func(m *AgentPaneModel) {
				m.SetStatus(event.StatusThinking)
				m.SetInputActive(true)
			},
			"nib is working",
		},
		{
			"focused with content shows send hint",
			func(m *AgentPaneModel) {
				m.SetInputActive(true)
				m.input.SetContent("typing something")
			},
			"enter send",
		},
		{
			"idle empty shows placeholder",
			func(m *AgentPaneModel) {},
			"ask nib something",
		},
		{
			"idle with content falls through to slash hint",
			func(m *AgentPaneModel) {
				m.input.SetContent("leftover")
			},
			"Ctrl+G",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := beatPane()
			tc.setup(m)
			hint := m.inputStateHintLine()
			if !strings.Contains(hint, tc.wantSubs) {
				t.Errorf("hint = %q; want substring %q", hint, tc.wantSubs)
			}
		})
	}
}

// TestInputStateDividerColor_StateAware verifies the hairline divider
// above the input takes its color from input state. Catches a
// regression where the divider falls back to dim gray during focus or
// streaming — losing the cheapest state indicator in the pane.
func TestInputStateDividerColor_StateAware(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*AgentPaneModel)
		want  string
	}{
		{
			"idle → DividerActive",
			func(m *AgentPaneModel) {},
			"#333",
		},
		{
			"focused → Accent",
			func(m *AgentPaneModel) { m.SetInputActive(true) },
			"#e879a0",
		},
		{
			"running → Warning",
			func(m *AgentPaneModel) { m.SetStatus(event.StatusThinking) },
			"#EF9F27",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := beatPane()
			tc.setup(m)
			got := m.inputStateDividerColor()
			// Compare via the corresponding theme constant — keeps the
			// test resilient to representation changes inside lipgloss.
			var want any
			switch tc.want {
			case "#333":
				want = theme.DividerActive
			case "#e879a0":
				want = theme.Accent
			case "#EF9F27":
				want = theme.Warning
			}
			if got != want {
				t.Errorf("divider color = %v; want %v (%s)", got, want, tc.want)
			}
		})
	}
}
