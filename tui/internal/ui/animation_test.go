package ui

import (
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
)

func newTestEditorForAnim(content string) *editor.Editor {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	return editor.New(buf)
}

func TestNewAnimationContext(t *testing.T) {
	tests := []struct {
		name          string
		wpm           int
		wantCharDelay time.Duration
	}{
		{
			name:          "default WPM",
			wpm:           0,
			wantCharDelay: time.Minute / time.Duration(defaultTypingWPM*avgCharsPerWord),
		},
		{
			name:          "custom WPM 600",
			wpm:           600,
			wantCharDelay: time.Minute / time.Duration(600*avgCharsPerWord),
		},
		{
			name:          "negative WPM uses default",
			wpm:           -1,
			wantCharDelay: time.Minute / time.Duration(defaultTypingWPM*avgCharsPerWord),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEditorForAnim("hello world")
			ie := e.BeginIncrementalEdit(0, 5, 6) // delete " world"
			ac := newAnimationContext(ie, "earth", tt.wpm)
			if ac.state != animTyping {
				t.Errorf("state = %d, want %d", ac.state, animTyping)
			}
			if ac.charDelay != tt.wantCharDelay {
				t.Errorf("charDelay = %v, want %v", ac.charDelay, tt.wantCharDelay)
			}
			if ac.newlineDelay != tt.wantCharDelay*4 {
				t.Errorf("newlineDelay = %v, want %v", ac.newlineDelay, tt.wantCharDelay*4)
			}
			ie.Abort()
		})
	}
}

func TestAnimationContextNextChar(t *testing.T) {
	e := newTestEditorForAnim("xx")
	ie := e.BeginIncrementalEdit(0, 0, 2)
	ac := newAnimationContext(ie, "ab", 300)

	if ac.done() {
		t.Fatal("should not be done before typing")
	}

	r1 := ac.nextChar()
	if r1 != 'a' {
		t.Errorf("first char = %c, want 'a'", r1)
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

	ie.Abort()
}

func TestAnimationContextDelayForChar(t *testing.T) {
	e := newTestEditorForAnim("xx")
	ie := e.BeginIncrementalEdit(0, 0, 2)
	ac := newAnimationContext(ie, "a\n", 300)

	normalDelay := ac.delayForChar('a')
	newlineDelay := ac.delayForChar('\n')

	if newlineDelay != normalDelay*4 {
		t.Errorf("newline delay = %v, want %v (4x normal)", newlineDelay, normalDelay*4)
	}

	ie.Abort()
}

func TestAnimationContextEmptyReplace(t *testing.T) {
	e := newTestEditorForAnim("old")
	ie := e.BeginIncrementalEdit(0, 0, 3)
	ac := newAnimationContext(ie, "", 300)
	if !ac.done() {
		t.Error("empty replace should be done immediately")
	}
	ie.Complete()
}

func TestAnimationContextPosition(t *testing.T) {
	e := newTestEditorForAnim("hello world")
	ie := e.BeginIncrementalEdit(0, 6, 5) // delete "world"
	ac := newAnimationContext(ie, "earth", 300)

	// Position starts at the edit location.
	line, col := ac.position()
	if line != 0 || col != 6 {
		t.Errorf("start position = %d:%d, want 0:6", line, col)
	}

	// Type "ea" and check position advances.
	ac.nextChar() // 'e'
	ie.InsertChar('e')
	ac.nextChar() // 'a'
	ie.InsertChar('a')

	line, col = ac.position()
	if line != 0 || col != 8 {
		t.Errorf("after 2 chars position = %d:%d, want 0:8", line, col)
	}

	ie.Abort()
}
