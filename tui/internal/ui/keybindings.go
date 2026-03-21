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

// Keymap holds all keybindings. Uses reverse lookup maps for
// deterministic O(1) matching (no map iteration order dependency).
type Keymap struct {
	byType   map[tea.KeyType]Action
	byString map[string]Action
}

// DefaultKeymap returns the macOS-first keymap.
func DefaultKeymap() *Keymap {
	km := &Keymap{
		byType: map[tea.KeyType]Action{
			tea.KeyCtrlQ:  ActionQuit,
			tea.KeyCtrlS:  ActionSave,
			tea.KeyCtrlZ:  ActionUndo,
			tea.KeyCtrlY:  ActionRedo,
			tea.KeyCtrlC:  ActionCopy,
			tea.KeyCtrlX:  ActionCut,
			tea.KeyCtrlV:  ActionPaste,
			tea.KeyCtrlA:  ActionSelectAll,
			tea.KeyCtrlO:  ActionAgentApprove,
			tea.KeyEscape: ActionAgentReject,
			tea.KeyCtrlN:  ActionAgentContinue,
		},
		byString: map[string]Action{
			"ctrl+g": ActionAgentStart,
		},
	}
	return km
}

// Match returns the action for a key event, or ActionNone. O(1) lookup.
func (km *Keymap) Match(msg tea.KeyMsg) Action {
	// String-based match first (more specific)
	if s := msg.String(); s != "" {
		if action, ok := km.byString[s]; ok {
			return action
		}
	}
	// Type-based match
	if action, ok := km.byType[msg.Type]; ok {
		return action
	}
	return ActionNone
}
