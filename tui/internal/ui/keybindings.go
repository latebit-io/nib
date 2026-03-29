package ui

import tea "charm.land/bubbletea/v2"

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
	ActionOpenPalette
	ActionToggleProject
	ActionGoToDefinition
	ActionGoBack
	ActionHover
)

// keyBinding represents a key combination mapped to an action.
type keyBinding struct {
	code rune
	mod  tea.KeyMod
}

// Keymap holds all keybindings. Uses reverse lookup maps for
// deterministic O(1) matching (no map iteration order dependency).
type Keymap struct {
	bindings map[keyBinding]Action
	byString map[string]Action
}

// DefaultKeymap returns the macOS-first keymap.
func DefaultKeymap() *Keymap {
	km := &Keymap{
		bindings: map[keyBinding]Action{
			{code: 'q', mod: tea.ModCtrl}: ActionQuit,
			{code: 's', mod: tea.ModCtrl}: ActionSave,
			{code: 'z', mod: tea.ModCtrl}: ActionUndo,
			{code: 'y', mod: tea.ModCtrl}: ActionRedo,
			{code: 'c', mod: tea.ModCtrl}: ActionCopy,
			{code: 'x', mod: tea.ModCtrl}: ActionCut,
			{code: 'v', mod: tea.ModCtrl}: ActionPaste,
			{code: 'a', mod: tea.ModCtrl}: ActionSelectAll,
			{code: 'o', mod: tea.ModCtrl}: ActionAgentApprove,
			{code: tea.KeyEscape, mod: 0}: ActionAgentReject,
			{code: 'n', mod: tea.ModCtrl}: ActionAgentContinue,
			{code: ']', mod: tea.ModCtrl}: ActionGoToDefinition,
			{code: 't', mod: tea.ModCtrl}: ActionGoBack,
			{code: 'k', mod: tea.ModCtrl}: ActionHover,
		},
		byString: map[string]Action{
			"ctrl+g": ActionAgentStart,
			"ctrl+p": ActionOpenPalette,
			"ctrl+b": ActionToggleProject,
		},
	}
	return km
}

// Match returns the action for a key event, or ActionNone. O(1) lookup.
func (km *Keymap) Match(msg tea.KeyPressMsg) Action {
	// String-based match first (more specific)
	if s := msg.String(); s != "" {
		if action, ok := km.byString[s]; ok {
			return action
		}
	}
	// Binding-based match
	if action, ok := km.bindings[keyBinding{code: msg.Code, mod: msg.Mod}]; ok {
		return action
	}
	return ActionNone
}
