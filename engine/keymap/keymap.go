// Package keymap defines editor actions and their human-readable binding catalog.
// This package has zero framework dependencies — frontends import it to map
// their input events to Actions and to render help screens from the catalog.
package keymap

// Action represents an editor action that can be triggered by a keybinding.
type Action int

const (
	// ActionNone represents no action (zero value).
	ActionNone Action = iota

	// ActionFileStart moves the cursor to the beginning of the file.
	ActionFileStart
	// ActionFileEnd moves the cursor to the end of the file.
	ActionFileEnd
	// ActionGoToLineStart moves the cursor to the start of the current line.
	ActionGoToLineStart
	// ActionGoToLineEnd moves the cursor to the end of the current line.
	ActionGoToLineEnd

	// ActionSelectAll selects all text in the buffer.
	ActionSelectAll
	// ActionSelectLine selects the current line.
	ActionSelectLine
	// ActionSelectNext selects the next occurrence of the current selection.
	ActionSelectNext

	// ActionUndo undoes the last edit operation.
	ActionUndo
	// ActionRedo redoes the last undone operation.
	ActionRedo
	// ActionCopy copies the selection to the clipboard.
	ActionCopy
	// ActionCut cuts the selection to the clipboard.
	ActionCut
	// ActionPaste pastes from the clipboard.
	ActionPaste
	// ActionDeleteLine deletes the current line.
	ActionDeleteLine
	// ActionDuplicateLine duplicates the current line.
	ActionDuplicateLine
	// ActionSwapLineUp swaps the current line with the one above.
	ActionSwapLineUp
	// ActionSwapLineDown swaps the current line with the one below.
	ActionSwapLineDown
	// ActionIndent increases the indentation of the selection.
	ActionIndent
	// ActionOutdent decreases the indentation of the selection.
	ActionOutdent
	// ActionToggleComment toggles line comments on the selection.
	ActionToggleComment

	// ActionSave persists the current buffer to disk.
	ActionSave
	// ActionQuit exits the editor.
	ActionQuit

	// ActionAgentStart begins an agent conversation.
	ActionAgentStart
	// ActionAgentPlan starts the agent in planning mode.
	ActionAgentPlan
	// ActionAgentApprove approves the pending edit.
	ActionAgentApprove
	// ActionAgentReject rejects the pending edit.
	ActionAgentReject
	// ActionAgentContinue continues the agent after an approved edit.
	ActionAgentContinue
	// ActionDialCycle cycles the autonomy level dial (guided → collaborate → trust → guided).
	ActionDialCycle
	// ActionModelSelector opens the LLM model/profile selector overlay.
	ActionModelSelector
	// ActionStyleCycle cycles through available coding styles.
	ActionStyleCycle
	// ActionEvaluatorToggle toggles the style evaluator on/off.
	ActionEvaluatorToggle

	// ActionOpenPalette opens the command palette.
	ActionOpenPalette
	// ActionToggleProject toggles the project pane visibility.
	ActionToggleProject
	// ActionHelp opens the help overlay.
	ActionHelp
	// ActionFocusProject focuses the project pane.
	ActionFocusProject
	// ActionFocusEditor focuses the editor pane.
	ActionFocusEditor
	// ActionFocusAgent focuses the agent pane.
	ActionFocusAgent

	// Find actions open search interfaces.
	ActionFind          // ActionFind opens the in-file find bar.
	ActionFindReplace   // ActionFindReplace opens find bar with replace mode.
	ActionFindInProject // ActionFindInProject opens project-wide search.

	// Buffer actions switch between open buffers.
	ActionNextBuffer // ActionNextBuffer switches to the next open buffer.
	ActionPrevBuffer // ActionPrevBuffer switches to the previous open buffer.

	// ActionGoToDefinition jumps to the definition of the symbol under the cursor.
	ActionGoToDefinition
	// ActionGoBack returns to the previous cursor location.
	ActionGoBack
	// ActionHover shows hover information for the symbol under the cursor.
	ActionHover
)

// Category groups related bindings in the help screen.
type Category string

const (
	// CatNavigation groups cursor movement bindings.
	CatNavigation Category = "Navigation"
	// CatSelection groups text selection bindings.
	CatSelection Category = "Selection"
	// CatEditing groups content modification bindings.
	CatEditing Category = "Editing"
	// CatFile groups file-level operations (save, quit).
	CatFile Category = "File"
	// CatAgent groups AI assistant workflow bindings.
	CatAgent Category = "Agent"
	// CatView groups pane and UI visibility bindings.
	CatView Category = "View"
	// CatFind groups search and replace bindings.
	CatFind Category = "Find"
	// CatBuffers groups buffer switching bindings.
	CatBuffers Category = "Buffers"
	// CatLSP groups language server feature bindings.
	CatLSP Category = "Code Intelligence"
)

// Binding describes a single keybinding for display purposes.
type Binding struct {
	// Action is the editor action this binding triggers.
	Action Action
	// Label is the human-readable name (e.g., "Delete Line").
	Label string
	// Keys lists the display strings for the key combination (e.g., ["Ctrl+K"]).
	Keys []string
	// Category groups this binding in the help screen.
	Category Category
}

// DefaultBindings returns the full keybinding catalog, ordered by category.
// This is the single source of truth for help screens across all frontends.
func DefaultBindings() []Binding {
	return []Binding{
		// Navigation
		{ActionFileStart, "Go to file start", []string{"Ctrl+Home"}, CatNavigation},
		{ActionFileEnd, "Go to file end", []string{"Ctrl+End"}, CatNavigation},
		{ActionGoToLineStart, "Go to line start", []string{"Home"}, CatNavigation},
		{ActionGoToLineEnd, "Go to line end", []string{"End"}, CatNavigation},

		// Selection
		{ActionSelectAll, "Select all", []string{"Ctrl+A"}, CatSelection},
		{ActionSelectLine, "Select line", []string{"Ctrl+L"}, CatSelection},
		{ActionSelectNext, "Select next occurrence", []string{"Ctrl+D"}, CatSelection},

		// Editing
		{ActionUndo, "Undo", []string{"Ctrl+Z"}, CatEditing},
		{ActionRedo, "Redo", []string{"Ctrl+Y", "Ctrl+Shift+Z"}, CatEditing},
		{ActionCopy, "Copy", []string{"Ctrl+C"}, CatEditing},
		{ActionCut, "Cut", []string{"Ctrl+X"}, CatEditing},
		{ActionPaste, "Paste", []string{"Ctrl+V"}, CatEditing},
		{ActionDeleteLine, "Delete line", []string{"Ctrl+K"}, CatEditing},
		{ActionDuplicateLine, "Duplicate line", []string{"Alt+D", "Shift+Alt+Down"}, CatEditing},
		{ActionSwapLineUp, "Move line up", []string{"Alt+Up"}, CatEditing},
		{ActionSwapLineDown, "Move line down", []string{"Alt+Down"}, CatEditing},
		{ActionToggleComment, "Toggle comment", []string{"Ctrl+/"}, CatEditing},
		{ActionIndent, "Indent", []string{"Tab", "Ctrl+]"}, CatEditing},
		{ActionOutdent, "Outdent", []string{"Shift+Tab"}, CatEditing},

		// File
		{ActionSave, "Save", []string{"Ctrl+S"}, CatFile},
		{ActionQuit, "Quit", []string{"Ctrl+Q"}, CatFile},

		// Agent
		{ActionAgentStart, "Start agent", []string{"Ctrl+G"}, CatAgent},
		{ActionAgentPlan, "Plan mode", []string{"Alt+G"}, CatAgent},
		{ActionAgentApprove, "Approve edit", []string{"Ctrl+O"}, CatAgent},
		{ActionAgentReject, "Reject / Cancel", []string{"Escape"}, CatAgent},
		{ActionAgentContinue, "Continue agent", []string{"Ctrl+N"}, CatAgent},
		{ActionDialCycle, "Cycle autonomy dial", []string{"Alt+A"}, CatAgent},
		{ActionModelSelector, "Select model", []string{"Alt+M"}, CatAgent},
		{ActionStyleCycle, "Cycle coding style", []string{"Alt+S"}, CatAgent},
		{ActionEvaluatorToggle, "Toggle style evaluator", []string{"Alt+V", "Alt+R"}, CatAgent},

		// View
		{ActionOpenPalette, "Open file palette", []string{"Ctrl+P"}, CatView},
		{ActionToggleProject, "Toggle project pane", []string{"Ctrl+B"}, CatView},
		{ActionHelp, "Show help", []string{"F4"}, CatView},
		{ActionFocusProject, "Focus project pane", []string{"F1"}, CatView},
		{ActionFocusEditor, "Focus editor", []string{"F2"}, CatView},
		{ActionFocusAgent, "Focus agent pane", []string{"F3"}, CatView},

		// Find
		{ActionFind, "Find in file", []string{"Ctrl+F"}, CatFind},
		{ActionFindReplace, "Find and replace", []string{"Ctrl+H"}, CatFind},
		{ActionFindInProject, "Find in project", []string{"Ctrl+Shift+F"}, CatFind},

		// Buffers
		{ActionNextBuffer, "Next buffer", []string{"Ctrl+PageDown"}, CatBuffers},
		{ActionPrevBuffer, "Previous buffer", []string{"Ctrl+PageUp"}, CatBuffers},

		// Code Intelligence
		{ActionGoToDefinition, "Go to definition", []string{"F12"}, CatLSP},
		{ActionGoBack, "Go back", []string{"Ctrl+-", "Ctrl+T"}, CatLSP},
		{ActionHover, "Hover info", []string{"Alt+K"}, CatLSP},
	}
}

// CategoryOrder returns categories in the order they should appear in help screens.
func CategoryOrder() []Category {
	return []Category{
		CatNavigation,
		CatSelection,
		CatEditing,
		CatFile,
		CatAgent,
		CatView,
		CatBuffers,
		CatFind,
		CatLSP,
	}
}
