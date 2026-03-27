// Package lang defines the language service port interfaces and domain types.
// The engine core depends on these abstractions — never on concrete LSP types.
// Implementations (e.g., engine/lsp) satisfy these interfaces and are injected
// at construction time via main.go.
package lang

import "context"

// DocumentSyncer is the mandatory base interface for any language backend.
// Every language service must accept document lifecycle events so it can
// maintain an up-to-date view of the editor's buffer state.
type DocumentSyncer interface {
	// DidOpen notifies the backend that a document was opened.
	DidOpen(path string, languageID string, content string)

	// DidChange notifies the backend of incremental edits.
	DidChange(path string, changes []TextChange)

	// DidSave notifies the backend that a document was saved to disk.
	DidSave(path string)

	// DidClose notifies the backend that a document was closed.
	DidClose(path string)

	// Close shuts down the language service and all managed servers.
	Close() error
}

// --- Optional capability interfaces ---
// Consumers check via type assertion: if dp, ok := svc.(DiagnosticProvider); ok { ... }

// DiagnosticProvider supplies compiler errors, warnings, and hints.
// Notifications that diagnostics have changed arrive via event.DiagnosticsUpdated
// on the shared event channel. This interface is query-only.
type DiagnosticProvider interface {
	Diagnostics(path string) []Diagnostic
}

// DefinitionProvider resolves go-to-definition requests.
type DefinitionProvider interface {
	Definition(ctx context.Context, path string, line, col int) (Location, error)
}

// HoverProvider returns type information and documentation at a position.
type HoverProvider interface {
	Hover(ctx context.Context, path string, line, col int) (string, error)
}

// CompletionProvider supplies code completions.
type CompletionProvider interface {
	Complete(ctx context.Context, path string, line, col int) (*CompletionResult, error)
	CancelCompletion()
}

// --- Domain types ---

// TextChange represents an incremental edit to a document.
// Session converts buffer.ContentChange → lang.TextChange at the call site.
type TextChange struct {
	StartLine, StartCol int    // 0-indexed, rune-based
	EndLine, EndCol     int    // 0-indexed, rune-based (position before edit)
	Text                string // replacement text
	FullContent         bool   // true = Text is the entire buffer, range fields are zero
}

// Diagnostic represents a compiler error, warning, or hint.
type Diagnostic struct {
	StartLine, StartCol int
	EndLine, EndCol     int
	Severity            Severity
	Message             string
	Source              string // e.g. "gopls", "eslint"
	Code                string // e.g. "unusedvar"
}

// Severity classifies the importance of a diagnostic.
type Severity int

const (
	// SeverityError indicates a compilation error.
	SeverityError Severity = 1
	// SeverityWarning indicates a potential problem.
	SeverityWarning Severity = 2
	// SeverityInfo indicates informational output.
	SeverityInfo Severity = 3
	// SeverityHint indicates a style suggestion.
	SeverityHint Severity = 4
)

// Location represents a position in a file.
type Location struct {
	Path      string
	Line, Col int // 0-indexed, rune-based
}

// CompletionResult holds completion items from a language server.
type CompletionResult struct {
	Items        []CompletionItem
	IsIncomplete bool // server may have more items
}

// CompletionItem represents a single completion suggestion.
type CompletionItem struct {
	Label      string         // display text
	Kind       CompletionKind // function, variable, type, etc.
	Detail     string         // type signature or short description
	InsertText string         // text to insert (may differ from label)
}

// CompletionKind classifies a completion item.
type CompletionKind int

const (
	// CompletionFunction represents a function or method completion.
	CompletionFunction CompletionKind = iota + 1
	// CompletionVariable represents a variable completion.
	CompletionVariable
	// CompletionField represents a struct field completion.
	CompletionField
	// CompletionType represents a type completion.
	CompletionType
	// CompletionConstant represents a constant completion.
	CompletionConstant
	// CompletionMethod represents a method completion.
	CompletionMethod
	// CompletionModule represents a module/package completion.
	CompletionModule
	// CompletionProperty represents a property completion.
	CompletionProperty
	// CompletionKeyword represents a language keyword completion.
	CompletionKeyword
	// CompletionSnippet represents a snippet completion.
	CompletionSnippet
)
