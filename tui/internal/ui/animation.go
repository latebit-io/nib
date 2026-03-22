package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// animTickMsg is sent by tea.Tick to advance the animation by one character.
type animTickMsg struct{}

// animationContext holds all state for an in-progress animated edit.
// This is TUI-only state — the engine knows nothing about animation.
type animationContext struct {
	state animState

	// Agent cursor position (buffer coordinates, 0-indexed).
	line int
	col  int

	// The region being animated — used for collision detection.
	startLine int
	startCol  int

	// The replacement text, as runes, and how far we've typed.
	replaceRunes []rune
	typed        int

	// Timing
	charDelay    time.Duration
	newlineDelay time.Duration

	// Undo group tracking — BeginGroup is called before delete,
	// EndGroup after all chars are inserted (or on yield/cancel).
	groupOpen bool

	// Whether the agent yielded due to cursor collision.
	yielded bool
}

const (
	defaultTypingWPM = 300
	avgCharsPerWord  = 5
)

// newAnimationContext creates a context for animating an edit at the given position.
func newAnimationContext(line, col int, replace string, wpm int) *animationContext {
	if wpm <= 0 {
		wpm = defaultTypingWPM
	}
	charDelay := time.Minute / time.Duration(wpm*avgCharsPerWord)

	return &animationContext{
		state:        animDeleting,
		line:         line,
		col:          col,
		startLine:    line,
		startCol:     col,
		replaceRunes: []rune(replace),
		charDelay:    charDelay,
		newlineDelay: charDelay * 4,
	}
}

// done returns true when all replacement characters have been typed.
func (a *animationContext) done() bool {
	return a.typed >= len(a.replaceRunes)
}

// nextChar returns the next character to type and advances the cursor.
func (a *animationContext) nextChar() rune {
	if a.done() {
		return 0
	}
	r := a.replaceRunes[a.typed]
	a.typed++
	return r
}

// delayForChar returns the appropriate delay after inserting a character.
// Newlines get a longer pause.
func (a *animationContext) delayForChar(r rune) time.Duration {
	if r == '\n' {
		return a.newlineDelay
	}
	return a.charDelay
}

// scheduleNextTick returns a tea.Cmd that schedules the next animation tick.
func scheduleNextTick(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(_ time.Time) tea.Msg {
		return animTickMsg{}
	})
}
