package lsp

import "encoding/json"

// --- LSP protocol types (internal to this package) ---
// These map to the LSP spec JSON structures. Converted to/from lang.* types
// at the Manager boundary. Unexported names prevent leaking LSP concepts
// into the engine domain.

// lspPosition is a zero-indexed line/character position.
// Character offset is encoding-dependent (UTF-16 by default, UTF-32 if negotiated).
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// lspRange is a start/end position pair.
type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspTextDocumentIdentifier identifies a document by URI.
type lspTextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// lspVersionedTextDocumentIdentifier adds a version to the identifier.
type lspVersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// lspTextDocumentItem is a full document descriptor for didOpen.
type lspTextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// lspContentChangeEvent represents an incremental text change.
type lspContentChangeEvent struct {
	Range *lspRange `json:"range,omitempty"` // nil = full content replace
	Text  string    `json:"text"`
}

// lspDiagnostic represents a diagnostic (error, warning, etc.).
type lspDiagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity,omitempty"` // 1=Error, 2=Warning, 3=Info, 4=Hint
	Message  string   `json:"message"`
	Source   string   `json:"source,omitempty"`
	Code     any      `json:"code,omitempty"` // string or int
}

// lspPublishDiagnosticsParams is the params for textDocument/publishDiagnostics.
type lspPublishDiagnosticsParams struct {
	URI         string          `json:"uri"`
	Diagnostics []lspDiagnostic `json:"diagnostics"`
}

// lspLocation is a location in a document (for go-to-definition, etc.).
type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

// lspHover is the result of textDocument/hover.
type lspHover struct {
	Contents lspMarkupContent `json:"contents"`
}

// lspMarkupContent holds markdown or plaintext content.
type lspMarkupContent struct {
	Kind  string `json:"kind"` // "plaintext" or "markdown"
	Value string `json:"value"`
}

// lspCompletionList is the result of textDocument/completion.
type lspCompletionList struct {
	IsIncomplete bool                `json:"isIncomplete"`
	Items        []lspCompletionItem `json:"items"`
}

// lspCompletionItem is a single completion suggestion.
type lspCompletionItem struct {
	Label      string `json:"label"`
	Kind       int    `json:"kind,omitempty"`
	Detail     string `json:"detail,omitempty"`
	InsertText string `json:"insertText,omitempty"`
}

// --- Initialize types ---

// lspInitializeParams is the params for the initialize request.
type lspInitializeParams struct {
	ProcessID    int                   `json:"processId"`
	RootURI      string                `json:"rootUri"`
	Capabilities lspClientCapabilities `json:"capabilities"`
}

// lspClientCapabilities declares what the client supports.
type lspClientCapabilities struct {
	General      *lspGeneralCapabilities            `json:"general,omitempty"`
	TextDocument *lspTextDocumentClientCapabilities `json:"textDocument,omitempty"`
}

// lspGeneralCapabilities includes position encoding negotiation (LSP 3.17+).
type lspGeneralCapabilities struct {
	PositionEncodings []string `json:"positionEncodings,omitempty"`
}

// lspTextDocumentClientCapabilities declares text document capabilities.
type lspTextDocumentClientCapabilities struct {
	Synchronization    *lspTextDocumentSyncClientCapabilities   `json:"synchronization,omitempty"`
	Completion         *lspCompletionClientCapabilities         `json:"completion,omitempty"`
	Hover              *lspHoverClientCapabilities              `json:"hover,omitempty"`
	PublishDiagnostics *lspPublishDiagnosticsClientCapabilities `json:"publishDiagnostics,omitempty"`
}

// lspTextDocumentSyncClientCapabilities declares sync support.
type lspTextDocumentSyncClientCapabilities struct {
	DidSave bool `json:"didSave,omitempty"`
}

// lspCompletionClientCapabilities declares completion support.
type lspCompletionClientCapabilities struct{}

// lspHoverClientCapabilities declares hover support.
type lspHoverClientCapabilities struct{}

// lspPublishDiagnosticsClientCapabilities declares diagnostics support.
type lspPublishDiagnosticsClientCapabilities struct{}

// lspInitializeResult is the result of the initialize request.
type lspInitializeResult struct {
	Capabilities lspServerCapabilities `json:"capabilities"`
}

// lspServerCapabilities describes what the server supports.
type lspServerCapabilities struct {
	TextDocumentSync   json.RawMessage `json:"textDocumentSync,omitempty"`
	CompletionProvider json.RawMessage `json:"completionProvider,omitempty"`
	HoverProvider      json.RawMessage `json:"hoverProvider,omitempty"`
	DefinitionProvider json.RawMessage `json:"definitionProvider,omitempty"`
	CodeActionProvider json.RawMessage `json:"codeActionProvider,omitempty"`
	RenameProvider     json.RawMessage `json:"renameProvider,omitempty"`
	PositionEncoding   string          `json:"positionEncoding,omitempty"`
}

// --- didOpen / didChange / didSave / didClose params ---

// lspDidOpenParams is the params for textDocument/didOpen.
type lspDidOpenParams struct {
	TextDocument lspTextDocumentItem `json:"textDocument"`
}

// lspDidChangeParams is the params for textDocument/didChange.
type lspDidChangeParams struct {
	TextDocument   lspVersionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []lspContentChangeEvent            `json:"contentChanges"`
}

// lspDidSaveParams is the params for textDocument/didSave.
type lspDidSaveParams struct {
	TextDocument lspTextDocumentIdentifier `json:"textDocument"`
}

// lspDidCloseParams is the params for textDocument/didClose.
type lspDidCloseParams struct {
	TextDocument lspTextDocumentIdentifier `json:"textDocument"`
}

// --- Position encoding constants ---

const (
	positionEncodingUTF32 = "utf-32"
	positionEncodingUTF16 = "utf-16"
)
