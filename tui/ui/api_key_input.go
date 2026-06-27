package ui

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
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
