package ui

import (
	"strings"

	kitcmd "github.com/latebit-io/nib/kit/command"
	"github.com/mattn/go-runewidth"
)

// maxCommandCompletionVisible caps the slash-completion popup height.
const maxCommandCompletionVisible = 8

// commandCandidate is one selectable command in the slash-completion
// popup: the canonical name (without leading slash) and its one-line
// description.
type commandCandidate struct {
	name string
	desc string
}

// commandCompletion is the agent input's slash-command autocomplete
// state. It activates while the user is typing a command name — input
// is "/" followed by non-space, with no space yet — and lists matching
// commands so they are discoverable without being memorized. Skills and
// tools are NOT here: they are model-invoked, not user-typed, so they
// surface via /capabilities, not completion.
type commandCompletion struct {
	active   bool
	matches  []commandCandidate
	selected int
	scroll   int
}

// commandNamePrefix returns the command-name prefix the user is typing
// and whether the input is in command-name-completion position — true
// only when content is "/" followed by non-space text with no space or
// newline yet (still naming the command, not typing arguments).
func commandNamePrefix(content string) (prefix string, ok bool) {
	if !strings.HasPrefix(content, "/") {
		return "", false
	}
	rest := content[1:]
	if strings.ContainsAny(rest, " \t\n") {
		return "", false
	}
	return rest, true
}

// refresh recomputes the popup for the given input content against the
// registry, dismissing when the input is not in command-name position
// or no command name has the typed prefix. reg.List() is already sorted
// by name, so the popup order is stable.
func (c *commandCompletion) refresh(content string, reg *kitcmd.Registry) {
	if reg == nil {
		c.dismiss()
		return
	}
	prefix, ok := commandNamePrefix(content)
	if !ok {
		c.dismiss()
		return
	}
	lower := strings.ToLower(prefix)
	var matches []commandCandidate
	for _, cmd := range reg.List() {
		d := cmd.Definition()
		if strings.HasPrefix(strings.ToLower(d.Name), lower) {
			matches = append(matches, commandCandidate{name: d.Name, desc: d.Description})
		}
	}
	if len(matches) == 0 {
		c.dismiss()
		return
	}
	c.matches = matches
	c.active = true
	c.selected = 0
	c.scroll = 0
}

// dismiss closes the popup and clears its state.
func (c *commandCompletion) dismiss() {
	c.active = false
	c.matches = nil
	c.selected = 0
	c.scroll = 0
}

// selectNext / selectPrev move the highlight, wrapping at the ends.
func (c *commandCompletion) selectNext() {
	if !c.active || len(c.matches) == 0 {
		return
	}
	c.selected = (c.selected + 1) % len(c.matches)
	c.clampScroll()
}

func (c *commandCompletion) selectPrev() {
	if !c.active || len(c.matches) == 0 {
		return
	}
	c.selected = (c.selected - 1 + len(c.matches)) % len(c.matches)
	c.clampScroll()
}

func (c *commandCompletion) clampScroll() {
	c.scroll = clampScrollOffset(c.selected, c.scroll, maxCommandCompletionVisible)
}

// selectedName returns the highlighted command's canonical name.
func (c *commandCompletion) selectedName() (string, bool) {
	if !c.active || c.selected < 0 || c.selected >= len(c.matches) {
		return "", false
	}
	return c.matches[c.selected].name, true
}

// acceptCommandCompletion replaces the input with the highlighted
// command name plus a trailing space (ready for arguments) and closes
// the popup. No-op when nothing is highlighted.
func (m *AgentPaneModel) acceptCommandCompletion() {
	name, ok := m.cmdComplete.selectedName()
	if !ok {
		m.cmdComplete.dismiss()
		return
	}
	m.input.SetContent("/" + name + " ")
	m.recomputeInputLayout()
	m.cmdComplete.dismiss()
}

// overlayCommandCompletion paints the slash-completion popup into the
// fill rows directly above the input separator. Mirrors
// [AgentPaneModel.overlayScrollbar]: it overwrites already-composed rows
// rather than reserving layout height, so the popup floats over the
// bottom of the timeline and vanishes the moment it is dismissed.
func (m *AgentPaneModel) overlayCommandCompletion(output []string) {
	if !m.cmdComplete.active || len(m.cmdComplete.matches) == 0 {
		return
	}
	sepRow := m.inputAreaStartRow - 1 // the hairline divider row
	if sepRow <= 0 {
		return
	}
	n := len(m.cmdComplete.matches)
	if n > maxCommandCompletionVisible {
		n = maxCommandCompletionVisible
	}
	startRow := sepRow - n
	if startRow < 0 {
		n += startRow // shrink to what fits above the divider
		startRow = 0
	}
	for i := 0; i < n; i++ {
		idx := m.cmdComplete.scroll + i
		if idx >= len(m.cmdComplete.matches) {
			break
		}
		row := startRow + i
		if row < 0 || row >= len(output) {
			continue
		}
		output[row] = m.renderCommandCompletionLine(m.cmdComplete.matches[idx], idx == m.cmdComplete.selected)
	}
}

// renderCommandCompletionLine formats one popup row, full-width so it
// cleanly overwrites the underlying timeline row.
func (m *AgentPaneModel) renderCommandCompletionLine(cand commandCandidate, selected bool) string {
	const nameCol = 16
	name := "/" + cand.name
	text := "  " + name
	if cand.desc != "" {
		if w := runewidth.StringWidth(text); w < nameCol {
			text += strings.Repeat(" ", nameCol-w)
		} else {
			text += "  "
		}
		text += sanitizeInlineDisplay(cand.desc)
	}
	text = runewidth.Truncate(text, m.width, "…")
	if w := runewidth.StringWidth(text); w < m.width {
		text += strings.Repeat(" ", m.width-w)
	}
	if selected {
		return completionSelectedStyle.Render(text)
	}
	return completionStyle.Render(text)
}
