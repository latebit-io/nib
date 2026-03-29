package ui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/junto/engine/keymap"
)

// Action is an alias for keymap.Action — keeps TUI code concise.
type Action = keymap.Action

// Re-export action constants so TUI code doesn't need to import keymap directly.
const (
	ActionNone           = keymap.ActionNone
	ActionQuit           = keymap.ActionQuit
	ActionSave           = keymap.ActionSave
	ActionUndo           = keymap.ActionUndo
	ActionRedo           = keymap.ActionRedo
	ActionCopy           = keymap.ActionCopy
	ActionCut            = keymap.ActionCut
	ActionPaste          = keymap.ActionPaste
	ActionSelectAll      = keymap.ActionSelectAll
	ActionSelectLine     = keymap.ActionSelectLine
	ActionSelectNext     = keymap.ActionSelectNext
	ActionDeleteLine     = keymap.ActionDeleteLine
	ActionDuplicateLine  = keymap.ActionDuplicateLine
	ActionSwapLineUp     = keymap.ActionSwapLineUp
	ActionSwapLineDown   = keymap.ActionSwapLineDown
	ActionToggleComment  = keymap.ActionToggleComment
	ActionIndent         = keymap.ActionIndent
	ActionOutdent        = keymap.ActionOutdent
	ActionFileStart      = keymap.ActionFileStart
	ActionFileEnd        = keymap.ActionFileEnd
	ActionAgentStart     = keymap.ActionAgentStart
	ActionAgentApprove   = keymap.ActionAgentApprove
	ActionAgentReject    = keymap.ActionAgentReject
	ActionAgentContinue  = keymap.ActionAgentContinue
	ActionOpenPalette    = keymap.ActionOpenPalette
	ActionToggleProject  = keymap.ActionToggleProject
	ActionHelp           = keymap.ActionHelp
	ActionGoToDefinition = keymap.ActionGoToDefinition
	ActionGoBack         = keymap.ActionGoBack
	ActionHover          = keymap.ActionHover
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
			// File
			{code: 'q', mod: tea.ModCtrl}: ActionQuit,
			{code: 's', mod: tea.ModCtrl}: ActionSave,

			// Editing
			{code: 'z', mod: tea.ModCtrl}:        ActionUndo,
			{code: 'y', mod: tea.ModCtrl}:        ActionRedo,
			{code: 'c', mod: tea.ModCtrl}:        ActionCopy,
			{code: 'x', mod: tea.ModCtrl}:        ActionCut,
			{code: 'v', mod: tea.ModCtrl}:        ActionPaste,
			{code: 'k', mod: tea.ModCtrl}:        ActionDeleteLine,
			{code: 'd', mod: tea.ModAlt}:         ActionDuplicateLine,
			{code: tea.KeyUp, mod: tea.ModAlt}:   ActionSwapLineUp,
			{code: tea.KeyDown, mod: tea.ModAlt}: ActionSwapLineDown,

			// Selection
			{code: 'a', mod: tea.ModCtrl}: ActionSelectAll,
			{code: 'l', mod: tea.ModCtrl}: ActionSelectLine,
			{code: 'd', mod: tea.ModCtrl}: ActionSelectNext,

			// Navigation
			{code: tea.KeyHome, mod: tea.ModCtrl}:  ActionFileStart,
			{code: tea.KeyEnd, mod: tea.ModCtrl}:   ActionFileEnd,
			{code: tea.KeyUp, mod: tea.ModSuper}:   ActionFileStart, // Cmd+Up (Kitty)
			{code: tea.KeyDown, mod: tea.ModSuper}: ActionFileEnd,   // Cmd+Down (Kitty)

			// Agent
			{code: 'o', mod: tea.ModCtrl}: ActionAgentApprove,
			{code: tea.KeyEscape, mod: 0}: ActionAgentReject,
			{code: 'n', mod: tea.ModCtrl}: ActionAgentContinue,

			// LSP
			{code: ']', mod: tea.ModCtrl}: ActionGoToDefinition,
			{code: 't', mod: tea.ModCtrl}: ActionGoBack,
			{code: 'k', mod: tea.ModAlt}:  ActionHover,
		},
		byString: map[string]Action{
			"ctrl+g": ActionAgentStart,
			"ctrl+p": ActionOpenPalette,
			"ctrl+b": ActionToggleProject,
			"ctrl+/": ActionToggleComment,
			"ctrl+_": ActionToggleComment, // some terminals send Ctrl+/ as Ctrl+_
			"f1":     ActionHelp,
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
