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
	ActionGoToLineStart  = keymap.ActionGoToLineStart
	ActionGoToLineEnd    = keymap.ActionGoToLineEnd
	ActionAgentStart     = keymap.ActionAgentStart
	ActionAgentPlan      = keymap.ActionAgentPlan
	ActionAgentApprove   = keymap.ActionAgentApprove
	ActionAgentReject    = keymap.ActionAgentReject
	ActionAgentContinue  = keymap.ActionAgentContinue
	ActionOpenPalette    = keymap.ActionOpenPalette
	ActionToggleProject  = keymap.ActionToggleProject
	ActionHelp           = keymap.ActionHelp
	ActionFocusProject   = keymap.ActionFocusProject
	ActionFocusEditor    = keymap.ActionFocusEditor
	ActionFocusAgent     = keymap.ActionFocusAgent
	ActionFind           = keymap.ActionFind
	ActionFindReplace    = keymap.ActionFindReplace
	ActionGoToDefinition = keymap.ActionGoToDefinition
	ActionGoBack         = keymap.ActionGoBack
	ActionHover          = keymap.ActionHover
	ActionNextBuffer     = keymap.ActionNextBuffer
	ActionPrevBuffer     = keymap.ActionPrevBuffer
	ActionFindInProject  = keymap.ActionFindInProject
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

			// Editing (VS Code compatible)
			{code: 'z', mod: tea.ModCtrl}:                       ActionUndo,
			{code: 'y', mod: tea.ModCtrl}:                       ActionRedo,
			{code: 'z', mod: tea.ModCtrl | tea.ModShift}:        ActionRedo, // Ctrl+Shift+Z (VS Code)
			{code: 'Z', mod: tea.ModCtrl | tea.ModShift}:        ActionRedo, // some terminals send uppercase with Shift
			{code: 'c', mod: tea.ModCtrl}:                       ActionCopy,
			{code: 'x', mod: tea.ModCtrl}:                       ActionCut,
			{code: 'v', mod: tea.ModCtrl}:                       ActionPaste,
			{code: 'k', mod: tea.ModCtrl}:                       ActionDeleteLine,
			{code: 'd', mod: tea.ModAlt}:                        ActionDuplicateLine,
			{code: tea.KeyDown, mod: tea.ModAlt | tea.ModShift}: ActionDuplicateLine, // Shift+Alt+Down (VS Code)
			{code: tea.KeyUp, mod: tea.ModAlt}:                  ActionSwapLineUp,    // Alt+Up (VS Code)
			{code: tea.KeyDown, mod: tea.ModAlt}:                ActionSwapLineDown,  // Alt+Down (VS Code)
			{code: ']', mod: tea.ModCtrl}:                       ActionIndent,        // Ctrl+] (VS Code indent)

			// Selection
			{code: 'a', mod: tea.ModCtrl}: ActionSelectAll,
			{code: 'l', mod: tea.ModCtrl}: ActionSelectLine,
			{code: 'd', mod: tea.ModCtrl}: ActionSelectNext,

			// Navigation
			{code: tea.KeyHome, mod: tea.ModCtrl}:                 ActionFileStart,
			{code: tea.KeyEnd, mod: tea.ModCtrl}:                  ActionFileEnd,
			{code: tea.KeyUp, mod: tea.ModSuper}:                  ActionFileStart,     // Cmd+Up (Kitty)
			{code: tea.KeyDown, mod: tea.ModSuper}:                ActionFileEnd,       // Cmd+Down (Kitty)
			{code: tea.KeyUp, mod: tea.ModSuper | tea.ModShift}:   ActionFileStart,     // Cmd+Shift+Up (Kitty)
			{code: tea.KeyDown, mod: tea.ModSuper | tea.ModShift}: ActionFileEnd,       // Cmd+Shift+Down (Kitty)
			{code: tea.KeyLeft, mod: tea.ModSuper}:                ActionGoToLineStart, // Cmd+Left (Kitty)
			{code: tea.KeyRight, mod: tea.ModSuper}:               ActionGoToLineEnd,   // Cmd+Right (Kitty)
			{code: '-', mod: tea.ModCtrl}:                         ActionGoBack,        // Ctrl+- (VS Code)

			// Agent
			{code: 'g', mod: tea.ModCtrl | tea.ModShift}: ActionAgentPlan,
			{code: 'G', mod: tea.ModCtrl | tea.ModShift}: ActionAgentPlan, // some terminals send uppercase with Shift
			{code: 'o', mod: tea.ModCtrl}:                ActionAgentApprove,
			{code: tea.KeyEscape, mod: 0}:                ActionAgentReject,
			{code: 'n', mod: tea.ModCtrl}:                ActionAgentContinue,

			// Find
			{code: 'f', mod: tea.ModCtrl}:                ActionFind,
			{code: 'h', mod: tea.ModCtrl}:                ActionFindReplace,
			{code: 'f', mod: tea.ModCtrl | tea.ModShift}: ActionFindInProject,
			{code: 'F', mod: tea.ModCtrl | tea.ModShift}: ActionFindInProject, // some terminals send uppercase with Shift

			// Buffers
			{code: tea.KeyPgDown, mod: tea.ModCtrl}: ActionNextBuffer,
			{code: tea.KeyPgUp, mod: tea.ModCtrl}:   ActionPrevBuffer,

			// LSP
			{code: 't', mod: tea.ModCtrl}: ActionGoBack,
			{code: 'k', mod: tea.ModAlt}:  ActionHover,
		},
		byString: map[string]Action{
			"ctrl+g":       ActionAgentStart,
			"ctrl+shift+g": ActionAgentPlan,
			"ctrl+p":       ActionOpenPalette,
			"ctrl+b":       ActionToggleProject,
			"ctrl+/":       ActionToggleComment,
			"ctrl+_":       ActionToggleComment, // some terminals send Ctrl+/ as Ctrl+_
			"f4":           ActionHelp,
			"f1":           ActionFocusProject,
			"f2":           ActionFocusEditor,
			"f3":           ActionFocusAgent,
			"f12":          ActionGoToDefinition, // F12 (VS Code)
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
