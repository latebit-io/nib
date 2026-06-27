package ui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"
)

// apiKeyEnteredMsg delivers a user-entered API key for a profile.
type apiKeyEnteredMsg struct {
	profile string
	key     string
}

// maxAPIKeyBytes bounds the API-key input buffer so an oversized clipboard or
// paste payload can't grow it without limit (the textarea applies the same
// guard via TextArea.Paste). Real keys are far smaller; this is a DoS guard,
// not validation.
const maxAPIKeyBytes = 8 * 1024

// sanitizeKeyPaste strips line breaks and surrounding whitespace from pasted
// API-key text. A trailing newline (common when copying a key from a file or
// web page) would otherwise corrupt the stored credential.
func sanitizeKeyPaste(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return strings.TrimSpace(s)
}

// appendAPIKey appends src to the API-key buffer, capping the total at
// maxAPIKeyBytes and truncating any overflow on a valid UTF-8 boundary.
func appendAPIKey(dst, src string) string {
	if src == "" {
		return dst
	}
	remaining := maxAPIKeyBytes - len(dst)
	if remaining <= 0 {
		return dst
	}
	if len(src) > remaining {
		src = src[:remaining]
		for len(src) > 0 && !utf8.Valid([]byte(src)) {
			src = src[:len(src)-1]
		}
	}
	return dst + src
}

// isPasteChord reports whether a keypress is the clipboard-paste shortcut
// (Ctrl+V or, on terminals that forward the macOS Command key, Super+V).
func isPasteChord(msg tea.KeyPressMsg) bool {
	if msg.Code != 'v' && msg.Code != 'V' {
		return false
	}
	return msg.Mod&tea.ModCtrl != 0 || msg.Mod&tea.ModSuper != 0
}

// APIKeyInputModel encapsulates the inline API-key entry overlay. When active
// it replaces the agent pane input area with a masked credential prompt. All
// state is unexported — mutations go through Open/Cancel/Paste/Update so the
// parent's focus handling stays the single source of input-activation truth.
type APIKeyInputModel struct {
	active  bool
	profile string // profile the key is for
	buffer  string // accumulated key text (masked in display)
}

// IsActive reports whether API-key entry is currently collecting input.
func (s *APIKeyInputModel) IsActive() bool { return s.active }

// Open activates API-key entry for the given profile, clearing any prior buffer.
func (s *APIKeyInputModel) Open(profile string) {
	s.active = true
	s.profile = profile
	s.buffer = ""
}

// Cancel deactivates API-key entry and clears the buffer and profile.
func (s *APIKeyInputModel) Cancel() {
	s.active = false
	s.buffer = ""
	s.profile = ""
}

// Paste appends sanitized clipboard or bracketed-paste content to the buffer.
// API keys are single-line, so line breaks and surrounding whitespace are
// stripped; the total is capped at maxAPIKeyBytes.
func (s *APIKeyInputModel) Paste(content string) {
	s.buffer = appendAPIKey(s.buffer, sanitizeKeyPaste(content))
}

// Update handles a key event during API-key entry. clip is the system
// clipboard service (may be nil) used to service the Ctrl/Cmd+V chord on
// terminals that don't deliver bracketed paste. Returns a tea.Cmd carrying
// the entered key on Enter; Escape and all other keys return nil. The parent
// observes IsActive() after Update to restore input focus when entry ends.
func (s *APIKeyInputModel) Update(msg tea.KeyPressMsg, clip ClipboardService) tea.Cmd {
	switch msg.Code {
	case tea.KeyEscape:
		s.Cancel()
		return nil
	case tea.KeyEnter:
		profile := s.profile
		key := s.buffer
		s.Cancel()
		return func() tea.Msg {
			return apiKeyEnteredMsg{profile: profile, key: key}
		}
	case tea.KeyBackspace:
		// Delete the last rune, not the last byte — a pasted multi-byte
		// character would otherwise leave buffer as invalid UTF-8.
		if _, size := utf8.DecodeLastRuneInString(s.buffer); size > 0 {
			s.buffer = s.buffer[:len(s.buffer)-size]
		}
		return nil
	default:
		// Ctrl/Cmd+V — read the system clipboard directly. Terminals that
		// don't use bracketed paste deliver the paste shortcut as this key
		// chord (the content never arrives as a PasteMsg or as key text),
		// so the textarea's clipboard path must be mirrored here.
		if isPasteChord(msg) {
			if clip != nil {
				s.buffer = appendAPIKey(s.buffer, sanitizeKeyPaste(clip.Read()))
			}
			return nil
		}
		if msg.Text != "" {
			s.buffer = appendAPIKey(s.buffer, msg.Text)
		}
		return nil
	}
}

// Render writes the API-key prompt into output starting at *row, filling the
// input-area band [startRow, endRow). The first row shows the masked key with
// a cursor; the last row a key hint; intervening rows are blank.
func (s *APIKeyInputModel) Render(output []string, row *int, width, height, startRow, endRow int) {
	inputRows := endRow - startRow
	if inputRows < 1 {
		inputRows = 1
	}
	prompt := fmt.Sprintf(" API key for %s: ", s.profile)
	masked := strings.Repeat("*", utf8.RuneCountInString(s.buffer))
	cursor := agentCursorStyle.Render(" ")

	for i := range inputRows {
		if *row >= height {
			break
		}
		switch i {
		case 0:
			line := prompt + masked + cursor
			output[*row] = agentInputStyle.Render(padLineToWidth(line, width))
		case inputRows - 1:
			output[*row] = agentInputDim.Render(padLineToWidth(" Enter confirm · Esc cancel", width))
		default:
			output[*row] = strings.Repeat(" ", width)
		}
		*row++
	}
}

// padLineToWidth right-pads s with spaces to width cells, or truncates it (no
// ellipsis) when it already meets or exceeds width. Unlike padToWidth, it
// truncates the overflow case rather than returning s unchanged.
func padLineToWidth(s string, width int) string {
	w := runewidth.StringWidth(s)
	if w >= width {
		return runewidth.Truncate(s, width, "")
	}
	return s + strings.Repeat(" ", width-w)
}
