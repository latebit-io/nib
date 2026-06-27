package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Terminal pastes arrive as tea.PasteMsg, not key events. These tests lock in
// that the API-key prompt and the agent input both consume a paste.

func TestPaste_APIKeyInput_StripsNewlineAndStores(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	m.StartAPIKeyInput("fugu")

	// A key copied from a file/web page often carries a trailing newline
	// and surrounding whitespace; both must be stripped.
	m.Update(tea.PasteMsg{Content: "  sk-abc123\n"})

	if got := m.apiKeyInput.buffer; got != "sk-abc123" {
		t.Fatalf("apiKeyBuffer = %q, want %q", got, "sk-abc123")
	}
	if !m.IsAPIKeyInputActive() {
		t.Fatal("API key input should still be active after paste")
	}
}

func TestPaste_APIKeyInput_AppendsToTypedPrefix(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	m.StartAPIKeyInput("fugu")
	m.handleAPIKeyInput(tea.KeyPressMsg{Code: 's', Text: "s"})

	m.Update(tea.PasteMsg{Content: "k-rest"})

	if got := m.apiKeyInput.buffer; got != "sk-rest" {
		t.Fatalf("apiKeyBuffer = %q, want %q", got, "sk-rest")
	}
}

func TestPaste_APIKeyInput_CtrlVReadsClipboard(t *testing.T) {
	// Terminals that don't use bracketed paste forward Cmd/Ctrl+V as a key
	// chord; the content never arrives as a PasteMsg or as key text, so the
	// handler must read the system clipboard itself.
	clip := &mockClipboard{content: "sk-from-clipboard\n"}
	m := NewAgentPaneModel(&Services{Clipboard: clip}, true)
	m.StartAPIKeyInput("fugu")

	m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})

	if got := m.apiKeyInput.buffer; got != "sk-from-clipboard" {
		t.Fatalf("apiKeyBuffer = %q, want %q", got, "sk-from-clipboard")
	}
}

func TestPaste_APIKeyInput_SuperVReadsClipboard(t *testing.T) {
	clip := &mockClipboard{content: "sk-cmd-v"}
	m := NewAgentPaneModel(&Services{Clipboard: clip}, true)
	m.StartAPIKeyInput("fugu")

	m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModSuper})

	if got := m.apiKeyInput.buffer; got != "sk-cmd-v" {
		t.Fatalf("apiKeyBuffer = %q, want %q", got, "sk-cmd-v")
	}
}

func TestPaste_AgentInput_InsertsIntoTextarea(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	m.SetInputActive(true)

	m.Update(tea.PasteMsg{Content: "hello world"})

	if got := m.input.Content(); !strings.Contains(got, "hello world") {
		t.Fatalf("input content = %q, want it to contain %q", got, "hello world")
	}
}

func TestPaste_NoActiveInput_NoOp(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	// Neither API-key nor input mode active.

	if cmd := m.handlePaste(tea.PasteMsg{Content: "ignored"}); cmd != nil {
		t.Fatal("paste with no active input should be a no-op")
	}
	if m.apiKeyInput.buffer != "" {
		t.Fatalf("apiKeyBuffer = %q, want empty", m.apiKeyInput.buffer)
	}
}

func TestPaste_APIKeyInput_CapsOversizedPayload(t *testing.T) {
	m := NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true)
	m.StartAPIKeyInput("fugu")

	// A payload far over the buffer cap must be truncated, not appended whole.
	m.Update(tea.PasteMsg{Content: strings.Repeat("a", maxAPIKeyBytes*2)})

	if got := len(m.apiKeyInput.buffer); got != maxAPIKeyBytes {
		t.Fatalf("apiKeyBuffer len = %d, want %d (capped)", got, maxAPIKeyBytes)
	}
}

// TestPaste_AppUpdate_RoutesToActiveInput locks the routing contract that
// lives in AppModel.handlePaste: a PasteMsg through AppModel.Update reaches
// the agent pane only when one of its inputs is active.
func TestPaste_AppUpdate_RoutesToActiveInput(t *testing.T) {
	newApp := func() *AppModel {
		return &AppModel{
			AgentPane: NewAgentPaneModel(&Services{Clipboard: &mockClipboard{}}, true),
		}
	}

	t.Run("API key input active → routed", func(t *testing.T) {
		m := newApp()
		m.AgentPane.StartAPIKeyInput("fugu")
		m.Update(tea.PasteMsg{Content: "sk-routed"})
		if got := m.AgentPane.apiKeyInput.buffer; got != "sk-routed" {
			t.Fatalf("apiKeyBuffer = %q, want %q", got, "sk-routed")
		}
	})

	t.Run("agent input active → routed", func(t *testing.T) {
		m := newApp()
		m.AgentPane.SetInputActive(true)
		m.Update(tea.PasteMsg{Content: "hello"})
		if got := m.AgentPane.input.Content(); !strings.Contains(got, "hello") {
			t.Fatalf("input content = %q, want it to contain %q", got, "hello")
		}
	})

	t.Run("no active input → not routed", func(t *testing.T) {
		m := newApp()
		m.Update(tea.PasteMsg{Content: "dropped"})
		if m.AgentPane.apiKeyInput.buffer != "" || m.AgentPane.input.Content() != "" {
			t.Fatal("paste should not reach any input when none is active")
		}
	})
}
