package ui

import (
	"testing"
	"time"
)

func TestNewAnimationContext(t *testing.T) {
	tests := []struct {
		name          string
		wpm           int
		replace       string
		wantState     animState
		wantCharDelay time.Duration
	}{
		{
			name:          "default WPM",
			wpm:           0,
			replace:       "hello",
			wantState:     animDeleting,
			wantCharDelay: time.Minute / time.Duration(defaultTypingWPM*avgCharsPerWord),
		},
		{
			name:          "custom WPM 600",
			wpm:           600,
			replace:       "hello",
			wantState:     animDeleting,
			wantCharDelay: time.Minute / time.Duration(600*avgCharsPerWord),
		},
		{
			name:          "negative WPM uses default",
			wpm:           -1,
			replace:       "hello",
			wantState:     animDeleting,
			wantCharDelay: time.Minute / time.Duration(defaultTypingWPM*avgCharsPerWord),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ac := newAnimationContext(5, 10, tt.replace, tt.wpm)
			if ac.state != tt.wantState {
				t.Errorf("state = %d, want %d", ac.state, tt.wantState)
			}
			if ac.line != 5 {
				t.Errorf("line = %d, want 5", ac.line)
			}
			if ac.col != 10 {
				t.Errorf("col = %d, want 10", ac.col)
			}
			if ac.charDelay != tt.wantCharDelay {
				t.Errorf("charDelay = %v, want %v", ac.charDelay, tt.wantCharDelay)
			}
			if ac.newlineDelay != tt.wantCharDelay*4 {
				t.Errorf("newlineDelay = %v, want %v", ac.newlineDelay, tt.wantCharDelay*4)
			}
		})
	}
}

func TestAnimationContextNextChar(t *testing.T) {
	ac := newAnimationContext(0, 0, "ab", 300)

	if ac.done() {
		t.Fatal("should not be done before typing")
	}

	r1 := ac.nextChar()
	if r1 != 'a' {
		t.Errorf("first char = %c, want 'a'", r1)
	}
	if ac.typed != 1 {
		t.Errorf("typed = %d, want 1", ac.typed)
	}

	r2 := ac.nextChar()
	if r2 != 'b' {
		t.Errorf("second char = %c, want 'b'", r2)
	}

	if !ac.done() {
		t.Error("should be done after typing all chars")
	}

	r3 := ac.nextChar()
	if r3 != 0 {
		t.Errorf("char after done = %c, want 0", r3)
	}
}

func TestAnimationContextDelayForChar(t *testing.T) {
	ac := newAnimationContext(0, 0, "a\n", 300)

	normalDelay := ac.delayForChar('a')
	newlineDelay := ac.delayForChar('\n')

	if newlineDelay != normalDelay*4 {
		t.Errorf("newline delay = %v, want %v (4x normal)", newlineDelay, normalDelay*4)
	}
}

func TestAnimationContextEmptyReplace(t *testing.T) {
	ac := newAnimationContext(0, 0, "", 300)
	if !ac.done() {
		t.Error("empty replace should be done immediately")
	}
}
