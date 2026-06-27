package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestRender_APIKeyPrompt_VisibleWhenNoAgent locks the fix for the prompt
// being invisible in the "No LLM configured" state — the very state in which a
// first-time user supplies a key. Before the fix the !hasAgent render branch
// painted only the model selector, so the prompt never appeared.
func TestRender_APIKeyPrompt_VisibleWhenNoAgent(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, false) // hasAgent=false
	m.SetSize(60, 24)
	m.StartAPIKeyInput("fugu")

	out := m.Render()
	if !strings.Contains(out, "API key for fugu") {
		t.Fatalf("no-agent render must show the API-key prompt; got:\n%s", out)
	}
	if !strings.Contains(out, "Esc cancel") {
		t.Fatalf("no-agent render must show the prompt hint; got:\n%s", out)
	}
}

// TestKey_AppUpdate_RoutesToAPIKeyPrompt locks the routing fix: typed keys must
// reach the API-key prompt through the top-level AppModel.Update regardless of
// which pane holds region focus (the prompt opens from the model selector,
// which does not focus the agent pane). Before the fix, keys fell through to
// the focused pane and leaked into the editor.
func TestKey_AppUpdate_RoutesToAPIKeyPrompt(t *testing.T) {
	m := &AppModel{AgentPane: NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)}
	m.AgentPane.StartAPIKeyInput("fugu")

	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := m.AgentPane.apiKeyInput.buffer; got != "x" {
		t.Fatalf("apiKeyInput.buffer = %q, want %q (key must route to the prompt)", got, "x")
	}

	// Escape routes through the same path and cancels.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.AgentPane.IsAPIKeyInputActive() {
		t.Fatal("Escape via AppModel.Update should cancel the API-key prompt")
	}
}

// These tests exercise APIKeyInputModel directly — the overlay is a
// self-contained sub-model, so its key handling is testable without the
// surrounding AgentPaneModel. Paste/clipboard routing through the parent is
// covered separately in agent_pane_paste_test.go.

func TestAPIKeyInput_OpenResetsState(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	s.buffer = "stale"

	s.Open("openai")

	if !s.IsActive() {
		t.Fatal("Open should activate the overlay")
	}
	if s.profile != "openai" {
		t.Fatalf("profile = %q, want %q", s.profile, "openai")
	}
	if s.buffer != "" {
		t.Fatalf("buffer = %q, want empty after Open", s.buffer)
	}
}

func TestAPIKeyInput_TypingAppendsRawText(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")

	for _, r := range "sk-1" {
		s.Update(tea.KeyPressMsg{Code: r, Text: string(r)}, nil)
	}

	if s.buffer != "sk-1" {
		t.Fatalf("buffer = %q, want %q", s.buffer, "sk-1")
	}
	if !s.IsActive() {
		t.Fatal("typing should not deactivate the overlay")
	}
}

func TestAPIKeyInput_Backspace(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	s.buffer = "ab"

	s.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}, nil)
	if s.buffer != "a" {
		t.Fatalf("buffer = %q, want %q", s.buffer, "a")
	}

	// Backspace on an empty buffer is a no-op (must not panic or underflow).
	s.buffer = ""
	s.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}, nil)
	if s.buffer != "" {
		t.Fatalf("buffer = %q, want empty", s.buffer)
	}
}

func TestAPIKeyInput_BackspaceAndMaskAreRuneAware(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	s.Paste("k🔑") // ASCII byte + 4-byte rune

	// Backspace must remove the whole multi-byte rune, not one byte, so the
	// buffer stays valid UTF-8.
	s.Update(tea.KeyPressMsg{Code: tea.KeyBackspace}, nil)
	if s.buffer != "k" {
		t.Fatalf("buffer = %q, want %q (whole rune deleted)", s.buffer, "k")
	}
	if !utf8.ValidString(s.buffer) {
		t.Fatalf("buffer is not valid UTF-8 after backspace: %q", s.buffer)
	}

	// Masking counts runes, not bytes: "é🔑" is 6 bytes but 2 runes → 2 stars.
	s.buffer = "é🔑"
	const width, height = 40, 10
	output := make([]string, height)
	row := 5
	s.Render(output, &row, width, height, 5, 8)
	first := ansi.Strip(output[5])
	if !strings.Contains(first, "fugu: **") || strings.Contains(first, "***") {
		t.Fatalf("masked row = %q, want exactly 2 mask glyphs (rune count, not byte count)", first)
	}
}

func TestAPIKeyInput_EnterEmitsKeyAndClears(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	s.buffer = "sk-secret"

	cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}, nil)
	if cmd == nil {
		t.Fatal("Enter should return a command carrying the entered key")
	}
	msg := cmd()
	entered, ok := msg.(apiKeyEnteredMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want apiKeyEnteredMsg", msg)
	}
	if entered.profile != "fugu" || entered.key != "sk-secret" {
		t.Fatalf("entered = %+v, want {profile:fugu key:sk-secret}", entered)
	}

	// Entry is consumed: overlay deactivated and secret wiped from state.
	if s.IsActive() {
		t.Fatal("overlay should deactivate after Enter")
	}
	if s.buffer != "" || s.profile != "" {
		t.Fatalf("residual state after Enter: buffer=%q profile=%q", s.buffer, s.profile)
	}
}

func TestAPIKeyInput_EscapeCancels(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	s.buffer = "sk-secret"

	cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEscape}, nil)
	if cmd != nil {
		t.Fatal("Escape should not emit a command")
	}
	if s.IsActive() {
		t.Fatal("overlay should deactivate after Escape")
	}
	if s.buffer != "" {
		t.Fatalf("buffer = %q, want cleared after Escape", s.buffer)
	}
}

func TestAPIKeyInput_PasteChordReadsClipboardSanitized(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	clip := &mockClipboard{content: "  sk-clip\n"}

	s.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}, clip)

	if s.buffer != "sk-clip" {
		t.Fatalf("buffer = %q, want %q (newline + whitespace stripped)", s.buffer, "sk-clip")
	}
}

func TestAPIKeyInput_PasteChordNilClipboardNoOp(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")

	// A nil clipboard (no service wired) must not panic.
	s.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModSuper}, nil)

	if s.buffer != "" {
		t.Fatalf("buffer = %q, want empty with nil clipboard", s.buffer)
	}
}

func TestAPIKeyInput_PasteSanitizesAndCaps(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")

	s.Paste("\tsk-xyz\r\n")
	if s.buffer != "sk-xyz" {
		t.Fatalf("buffer = %q, want %q", s.buffer, "sk-xyz")
	}

	s.buffer = ""
	s.Paste(strings.Repeat("a", maxAPIKeyBytes*2))
	if len(s.buffer) != maxAPIKeyBytes {
		t.Fatalf("buffer len = %d, want %d (capped)", len(s.buffer), maxAPIKeyBytes)
	}
}

func TestAPIKeyInput_RenderMasksBufferWithinBand(t *testing.T) {
	var s APIKeyInputModel
	s.Open("fugu")
	s.buffer = "ab" // two runes → two mask glyphs

	const width, height = 40, 10
	output := make([]string, height)
	row := 5
	s.Render(output, &row, width, height, 5, 8) // 3-row input band

	if row != 8 {
		t.Fatalf("row advanced to %d, want 8 (band end)", row)
	}
	first := ansi.Strip(output[5])
	if !strings.Contains(first, "API key for fugu:") {
		t.Fatalf("first row = %q, want the profile prompt", first)
	}
	if !strings.Contains(first, "**") {
		t.Fatalf("first row = %q, want masked buffer (**)", first)
	}
	if strings.Contains(first, "ab") {
		t.Fatalf("first row = %q, must not leak the raw key", first)
	}
	last := ansi.Strip(output[7])
	if !strings.Contains(last, "Esc cancel") {
		t.Fatalf("last row = %q, want the key hint", last)
	}
}
