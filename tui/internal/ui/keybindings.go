package ui

import tea "github.com/charmbracelet/bubbletea"

// Action represents an editor action that can be bound to a key.
type Action int

const (
	ActionNone Action = iota
	ActionQuit
	ActionSave
	ActionUndo
	ActionRedo
	ActionCopy
	ActionCut
	ActionPaste
	ActionSelectAll
	ActionAgentStart
	ActionAgentApprove
	ActionAgentReject
	ActionAgentContinue
)

// KeyBinding maps a key event to an action.
type KeyBinding struct {
	Type   tea.KeyType // for special keys (ctrl+q, etc.)
	String string      // for string-based matching ("ctrl+enter", etc.)
}

// Keymap holds all keybindings. Swap this out for different platforms.
type Keymap struct {
	bindings map[Action][]KeyBinding
}

// DefaultKeymap returns the macOS-first keymap.
func DefaultKeymap() *Keymap {
	km := &Keymap{
		bindings: map[Action][]KeyBinding{
			ActionQuit:          {{Type: tea.KeyCtrlQ}},
			ActionSave:          {{Type: tea.KeyCtrlS}},
			ActionUndo:          {{Type: tea.KeyCtrlZ}},
			ActionRedo:          {{Type: tea.KeyCtrlY}},
			ActionCopy:          {{Type: tea.KeyCtrlC}},
			ActionCut:           {{Type: tea.KeyCtrlX}},
			ActionPaste:         {{Type: tea.KeyCtrlV}},
			ActionSelectAll:     {{Type: tea.KeyCtrlA}},
			ActionAgentStart:    {{String: "ctrl+g"}},
			ActionAgentApprove:  {{Type: tea.KeyCtrlO}}, // Ctrl+O = approve
			ActionAgentReject:   {{Type: tea.KeyEscape}},
			ActionAgentContinue: {{Type: tea.KeyCtrlN}},
		},
	}
	return km
}

// Match returns the action for a key event, or ActionNone.
func (km *Keymap) Match(msg tea.KeyMsg) Action {
	for action, bindings := range km.bindings {
		for _, b := range bindings {
			if b.String != "" && msg.String() == b.String {
				return action
			}
			if b.String == "" && msg.Type == b.Type {
				return action
			}
		}
	}
	return ActionNone
}
