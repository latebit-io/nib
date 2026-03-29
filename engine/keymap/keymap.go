// Package keymap defines editor actions and their human-readable binding catalog.
// This package has zero framework dependencies — frontends import it to map
// their input events to Actions and to render help screens from the catalog.
package keymap

// Action represents an editor action that can be triggered by a keybinding.
type Action int

const (
	ActionNone Action = iota

	// Navigation
	ActionFileStart
	ActionFileEnd

	// Selection
	ActionSelectAll
	ActionSelectLine
	ActionSelectNext

	// Editing
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

	// File
	ActionSave
	ActionQuit

	// Agent
	ActionAgentStart
	ActionAgentApprove
	ActionAgentReject
	ActionAgentContinue

	// View
	ActionOpenPalette
	ActionToggleProject
	ActionHelp

	// LSP
	ActionGoToDefinition
	ActionGoBack
	ActionHover
)

// Category groups related bindings in the help screen.
type Category string

const (
	CatNavigation Category = "Navigation"
	CatSelection  Category = "Selection"
	CatEditing    Category = "Editing"
	CatFile       Category = "File"
	CatAgent      Category = "Agent"
	CatView       Category = "View"
	CatLSP        Category = "Code Intelligence"
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

		// Selection
		{ActionSelectAll, "Select all", []string{"Ctrl+A"}, CatSelection},
		{ActionSelectLine, "Select line", []string{"Ctrl+L"}, CatSelection},
		{ActionSelectNext, "Select next occurrence", []string{"Ctrl+D"}, CatSelection},

		// Editing
		{ActionUndo, "Undo", []string{"Ctrl+Z"}, CatEditing},
		{ActionRedo, "Redo", []string{"Ctrl+Y"}, CatEditing},
		{ActionCopy, "Copy", []string{"Ctrl+C"}, CatEditing},
		{ActionCut, "Cut", []string{"Ctrl+X"}, CatEditing},
		{ActionPaste, "Paste", []string{"Ctrl+V"}, CatEditing},
		{ActionDeleteLine, "Delete line", []string{"Ctrl+K"}, CatEditing},
		{ActionDuplicateLine, "Duplicate line", []string{"Alt+D"}, CatEditing},
		{ActionSwapLineUp, "Move line up", []string{"Alt+Up"}, CatEditing},
		{ActionSwapLineDown, "Move line down", []string{"Alt+Down"}, CatEditing},
		{ActionToggleComment, "Toggle comment", []string{"Ctrl+/"}, CatEditing},
		{ActionIndent, "Indent selection", []string{"Tab"}, CatEditing},
		{ActionOutdent, "Outdent selection", []string{"Shift+Tab"}, CatEditing},

		// File
		{ActionSave, "Save", []string{"Ctrl+S"}, CatFile},
		{ActionQuit, "Quit", []string{"Ctrl+Q"}, CatFile},

		// Agent
		{ActionAgentStart, "Start agent", []string{"Ctrl+G"}, CatAgent},
		{ActionAgentApprove, "Approve edit", []string{"Ctrl+O"}, CatAgent},
		{ActionAgentReject, "Reject / Cancel", []string{"Escape"}, CatAgent},
		{ActionAgentContinue, "Continue agent", []string{"Ctrl+N"}, CatAgent},

		// View
		{ActionOpenPalette, "Open file palette", []string{"Ctrl+P"}, CatView},
		{ActionToggleProject, "Toggle project pane", []string{"Ctrl+B"}, CatView},
		{ActionHelp, "Show help", []string{"F1"}, CatView},

		// Code Intelligence
		{ActionGoToDefinition, "Go to definition", []string{"Ctrl+]"}, CatLSP},
		{ActionGoBack, "Go back", []string{"Ctrl+T"}, CatLSP},
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
		CatLSP,
	}
}
