// Package keymap defines editor actions and their human-readable binding catalog.
// This package has zero framework dependencies — frontends import it to map
// their input events to Actions and to render help screens from the catalog.
package keymap

// Action represents an editor action that can be triggered by a keybinding.
type Action int

const (
	// ActionNone represents no action (zero value).
	ActionNone Action = iota

	// Navigation actions move the cursor within a buffer.
	ActionFileStart
	ActionFileEnd
	ActionGoToLineStart
	ActionGoToLineEnd

	// Selection actions extend or create text selections.
	ActionSelectAll
	ActionSelectLine
	ActionSelectNext

	// Editing actions modify buffer content.
	ActionUndo
	ActionRedo
	ActionCopy
	ActionCut
	ActionPaste
	ActionDeleteLine
	ActionDuplicateLine
	ActionSwapLineUp
	ActionSwapLineDown
	ActionIndent
	ActionOutdent
	ActionToggleComment

	// ActionSave persists the current buffer to disk.
	ActionSave
	// ActionQuit exits the editor.
	ActionQuit

	// Agent actions control the AI assistant workflow.
	ActionAgentStart
	ActionAgentPlan
	ActionAgentApprove
	ActionAgentReject
	ActionAgentContinue
	// ActionDialCycle cycles the autonomy level dial (guided → collaborate → trust → guided).
	ActionDialCycle
	// ActionModelSelector opens the LLM model/profile selector overlay.
	ActionModelSelector
	// ActionStyleCycle cycles through available coding styles.
	ActionStyleCycle
	// ActionEvaluatorToggle toggles the style evaluator on/off.
	ActionEvaluatorToggle

	// View actions control pane visibility and focus.
	ActionOpenPalette
	ActionToggleProject
	ActionHelp
	ActionFocusProject
	ActionFocusEditor
	ActionFocusAgent

	// Find actions open search interfaces.
	ActionFind          // ActionFind opens the in-file find bar.
	ActionFindReplace   // ActionFindReplace opens find bar with replace mode.
	ActionFindInProject // ActionFindInProject opens project-wide search.

	// Buffer actions switch between open buffers.
	ActionNextBuffer // ActionNextBuffer switches to the next open buffer.
	ActionPrevBuffer // ActionPrevBuffer switches to the previous open buffer.

	// LSP actions interact with language server features.
	ActionGoToDefinition
	ActionGoBack
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
	Action   Action
	Label    string   // human-readable name, e.g. "Delete Line"
	Keys     []string // display strings, e.g. ["Ctrl+K"]
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
