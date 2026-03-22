package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/editor"
)

// agentCursorInfo holds per-render agent cursor state for the rendering pipeline.
type agentCursorInfo struct {
	line  int
	col   int
	style lipgloss.Style
}

// animState tracks the phase of an animated agent edit.
type animState int

const (
	animIdle     animState = iota
	animDeleting           // deleting the old text (single frame)
	animTyping             // inserting replacement char-by-char
	animWaiting            // animation finished or yielded, waiting for dev
)

// animTickMsg is sent by tea.Tick to advance the animation by one frame.
type animTickMsg struct{}

// animationContext holds TUI-only state for an in-progress animated edit.
// Buffer mutations, position tracking, and per-tick advancement are all
// delegated to the engine's IncrementalEdit. This struct owns only the
// tick scheduling and visual state.
type animationContext struct {
	state animState

	// Engine-owned incremental edit — manages undo group, position tracking,
	// buffer mutations, and per-tick char advancement.
	edit *editor.IncrementalEdit

	// Whether the agent yielded due to cursor collision.
	yielded bool
}

const (
	defaultTypingWPM = 800
	avgCharsPerWord  = 5
	// tickInterval is the target time between animation frames.
	tickInterval = 16 * time.Millisecond // ~60fps
)

const (
	minTypingWPM = 30
	maxTypingWPM = 5000
)

// charsPerTick computes how many characters to insert per tick to hit the
// target WPM. This is called once at animation start.
func charsPerTick(wpm int) int {
	if wpm <= 0 {
		wpm = defaultTypingWPM
	}
	wpm = max(minTypingWPM, min(wpm, maxTypingWPM))
	charsPerSec := float64(wpm*avgCharsPerWord) / 60.0
	cpt := int(charsPerSec * tickInterval.Seconds())
	if cpt < 1 {
		cpt = 1
	}
	return cpt
}

// scheduleNextTick returns a tea.Cmd that schedules the next animation tick.
func scheduleNextTick() tea.Cmd {
	return tea.Tick(tickInterval, func(_ time.Time) tea.Msg {
		return animTickMsg{}
	})
}
