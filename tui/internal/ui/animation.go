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

// animTickMsg is sent by tea.Tick to advance the animation by one character.
type animTickMsg struct{}

// animationContext holds TUI-only state for an in-progress animated edit.
// Buffer mutations and undo group lifecycle are delegated to the engine's
// IncrementalEdit — this struct owns only timing and visual state.
type animationContext struct {
	state animState

	// Engine-owned incremental edit — manages undo group, position tracking,
	// and buffer mutations. Nil after Complete/Abort.
	edit *editor.IncrementalEdit

	// The replacement text, as runes, and how far we've typed.
	replaceRunes []rune
	typed        int

	// Timing
	charDelay    time.Duration
	newlineDelay time.Duration

	// Whether the agent yielded due to cursor collision.
	yielded bool
}

const (
	defaultTypingWPM = 500
	avgCharsPerWord  = 5
)

// newAnimationContext creates a context wrapping an engine IncrementalEdit.
const (
	minTypingWPM = 30
	maxTypingWPM = 2000
	minCharDelay = 5 * time.Millisecond
)

func newAnimationContext(ie *editor.IncrementalEdit, replace string, wpm int) *animationContext {
	if wpm <= 0 {
		wpm = defaultTypingWPM
	}
	wpm = max(minTypingWPM, min(wpm, maxTypingWPM))
	charDelay := time.Minute / time.Duration(wpm*avgCharsPerWord)
	charDelay = max(charDelay, minCharDelay)

	return &animationContext{
		state:        animTyping,
		edit:         ie,
		replaceRunes: []rune(replace),
		charDelay:    charDelay,
		newlineDelay: charDelay * 4,
	}
}

// done returns true when all replacement characters have been typed.
func (a *animationContext) done() bool {
	return a.typed >= len(a.replaceRunes)
}

// nextChar returns the next character to type. Does not advance — call
// edit.InsertChar separately so the engine owns the mutation.
func (a *animationContext) nextChar() rune {
	if a.done() {
		return 0
	}
	r := a.replaceRunes[a.typed]
	a.typed++
	return r
}

// position returns the current agent cursor position from the engine.
func (a *animationContext) position() (line, col int) {
	return a.edit.Position()
}

// startPosition returns where the edit began (for collision detection).
func (a *animationContext) startPosition() (line, col int) {
	return a.edit.StartPosition()
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
