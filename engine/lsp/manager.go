package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/lang"
)

// Manager implements lang.DocumentSyncer (and optional capability interfaces)
// by routing to per-language LSP servers. Servers are started lazily on the
// first DidOpen for a given language.
type Manager struct {
	configs     map[string]ServerConfig // languageID → config
	projectRoot string
	events      chan<- event.Event // shared event channel for diagnostic notifications

	mu       sync.RWMutex
	servers  map[string]*Server           // languageID → running server
	versions map[string]int               // file path → document version
	docLang  map[string]string            // file path → languageID (set at DidOpen)
	diags    map[string][]lang.Diagnostic // file path → diagnostics
}

// Compile-time interface checks.
var _ lang.DocumentSyncer = (*Manager)(nil)
var _ lang.DiagnosticProvider = (*Manager)(nil)
var _ lang.ReferenceProvider = (*Manager)(nil)
var _ lang.SymbolProvider = (*Manager)(nil)

// NewManager creates a Manager that routes to LSP servers based on language.
// Configs map language IDs to server configurations. The events channel
// receives DiagnosticsUpdated events when a server pushes new diagnostics.
// projectRoot is used as the workspace root for LSP initialization.
func NewManager(configs []ServerConfig, projectRoot string, events chan<- event.Event) *Manager {
	cfgMap := make(map[string]ServerConfig, len(configs))
	for _, cfg := range configs {
		cfgMap[cfg.LanguageID] = cfg
	}
	return &Manager{
		configs:     cfgMap,
		projectRoot: projectRoot,
		events:      events,
		servers:     make(map[string]*Server),
		versions:    make(map[string]int),
		docLang:     make(map[string]string),
		diags:       make(map[string][]lang.Diagnostic),
	}
}

// --- lang.DocumentSyncer ---

// DidOpen notifies the language server that a document was opened.
// Starts the server lazily if this is the first file for the language.
// The languageID is stored and used for all subsequent lifecycle events
// (DidChange, DidSave, DidClose) to ensure consistent server routing.
//
// State is updated optimistically before the notification because LSP
// notifications are fire-and-forget (no response). Rolling back on failure
// creates worse bugs: DidChange would silently drop all changes for a
// document with no docLang entry, and version gaps violate the LSP spec's
// monotonic version requirement. Transport failures are handled by
// server reconnection, not state rollback.
func (m *Manager) DidOpen(path string, languageID string, content string) {
	srv := m.serverFor(languageID)
	if srv == nil {
		return
	}

	m.mu.Lock()
	m.versions[path] = 1
	m.docLang[path] = languageID
	m.mu.Unlock()

	srv.DidOpen(pathToURI(path), languageID, 1, content)
}

// DidChange sends incremental changes to the language server.
// Routes to the server that handled DidOpen (not re-detected).
func (m *Manager) DidChange(path string, changes []lang.TextChange) {
	m.mu.RLock()
	languageID := m.docLang[path]
	srv, ok := m.servers[languageID]
	m.mu.RUnlock()
	if languageID == "" || !ok || srv == nil {
		return
	}

	m.mu.Lock()
	m.versions[path]++
	version := m.versions[path]
	m.mu.Unlock()

	srv.DidChange(pathToURI(path), version, changes)
}

// DidSave notifies the language server that a document was saved.
// Routes to the server that handled DidOpen (not re-detected).
func (m *Manager) DidSave(path string) {
	m.mu.RLock()
	languageID := m.docLang[path]
	srv, ok := m.servers[languageID]
	m.mu.RUnlock()
	if languageID == "" || !ok || srv == nil {
		return
	}

	srv.DidSave(pathToURI(path))
}

// DidClose notifies the language server that a document was closed.
// Routes to the server that handled DidOpen (not re-detected).
func (m *Manager) DidClose(path string) {
	m.mu.RLock()
	languageID := m.docLang[path]
	srv, ok := m.servers[languageID]
	m.mu.RUnlock()
	if languageID == "" || !ok || srv == nil {
		return
	}

	srv.DidClose(pathToURI(path))

	m.mu.Lock()
	delete(m.versions, path)
	delete(m.docLang, path)
	delete(m.diags, path)
	m.mu.Unlock()
}

// Close shuts down all managed servers.
func (m *Manager) Close() error {
	m.mu.Lock()
	servers := make([]*Server, 0, len(m.servers))
	for _, srv := range m.servers {
		servers = append(servers, srv)
	}
	m.servers = make(map[string]*Server)
	m.mu.Unlock()

	var firstErr error
	for _, srv := range servers {
		if err := srv.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// --- lang.FullContentSyncer ---

// NeedsFullContentSync returns true if any running server uses UTF-16
// position encoding (i.e., UTF-32 was not negotiated). In that case,
// incremental changes with rune-based positions would be incorrect for
// non-BMP characters. Callers should send full document content instead.
func (m *Manager) NeedsFullContentSync() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, srv := range m.servers {
		if srv.posEncoding != positionEncodingUTF32 {
			return true
		}
	}
	return false
}

// --- lang.DiagnosticProvider ---

// Diagnostics returns the current diagnostics for a file.
// Returns nil if no diagnostics are available.
func (m *Manager) Diagnostics(path string) []lang.Diagnostic {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.diags[path]
}

// --- lang.DefinitionProvider ---

// Definition resolves go-to-definition for a position.
func (m *Manager) Definition(ctx context.Context, path string, line, col int) (lang.Location, error) {
	srv, err := m.serverForPath(path)
	if err != nil {
		return lang.Location{}, err
	}

	params := struct {
		TextDocument lspTextDocumentIdentifier `json:"textDocument"`
		Position     lspPosition               `json:"position"`
	}{
		TextDocument: lspTextDocumentIdentifier{URI: pathToURI(path)},
		Position:     srv.toLSPPosition(line, col),
	}

	raw, err := srv.transport.Request(ctx, "textDocument/definition", params)
	if err != nil {
		return lang.Location{}, fmt.Errorf("definition request: %w", err)
	}

	// Response can be Location, Location[], or null.
	// Try single location first, then array.
	var loc lspLocation
	if err := json.Unmarshal(raw, &loc); err == nil && loc.URI != "" {
		l, c := srv.fromLSPPosition(loc.Range.Start)
		return lang.Location{Path: uriToPath(loc.URI), Line: l, Col: c}, nil
	}

	var locs []lspLocation
	if err := json.Unmarshal(raw, &locs); err == nil && len(locs) > 0 {
		l, c := srv.fromLSPPosition(locs[0].Range.Start)
		return lang.Location{Path: uriToPath(locs[0].URI), Line: l, Col: c}, nil
	}

	return lang.Location{}, fmt.Errorf("no definition found")
}

// --- lang.HoverProvider ---

// Hover returns type information and documentation at a position.
func (m *Manager) Hover(ctx context.Context, path string, line, col int) (string, error) {
	srv, err := m.serverForPath(path)
	if err != nil {
		return "", err
	}

	params := struct {
		TextDocument lspTextDocumentIdentifier `json:"textDocument"`
		Position     lspPosition               `json:"position"`
	}{
		TextDocument: lspTextDocumentIdentifier{URI: pathToURI(path)},
		Position:     srv.toLSPPosition(line, col),
	}

	raw, err := srv.transport.Request(ctx, "textDocument/hover", params)
	if err != nil {
		return "", fmt.Errorf("hover request: %w", err)
	}

	var hover lspHover
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", nil // no hover info
	}
	return hover.Contents.Value, nil
}

// --- lang.CompletionProvider ---

// Complete returns completions at a position.
func (m *Manager) Complete(ctx context.Context, path string, line, col int) (*lang.CompletionResult, error) {
	srv, err := m.serverForPath(path)
	if err != nil {
		return nil, err
	}

	params := struct {
		TextDocument lspTextDocumentIdentifier `json:"textDocument"`
		Position     lspPosition               `json:"position"`
	}{
		TextDocument: lspTextDocumentIdentifier{URI: pathToURI(path)},
		Position:     srv.toLSPPosition(line, col),
	}

	raw, err := srv.transport.Request(ctx, "textDocument/completion", params)
	if err != nil {
		return nil, fmt.Errorf("completion request: %w", err)
	}

	// LSP spec allows CompletionList or bare []CompletionItem.
	var list lspCompletionList
	if err := json.Unmarshal(raw, &list); err != nil {
		var items []lspCompletionItem
		if err2 := json.Unmarshal(raw, &items); err2 != nil {
			return nil, fmt.Errorf("unmarshal completions: %w", err)
		}
		list = lspCompletionList{Items: items}
	}

	items := make([]lang.CompletionItem, len(list.Items))
	for i, item := range list.Items {
		insertText := item.InsertText
		if insertText == "" {
			insertText = item.Label
		}
		items[i] = lang.CompletionItem{
			Label:      item.Label,
			Kind:       lang.CompletionKind(item.Kind),
			Detail:     item.Detail,
			InsertText: insertText,
		}
	}

	return &lang.CompletionResult{
		Items:        items,
		IsIncomplete: list.IsIncomplete,
	}, nil
}

// CancelCompletion cancels an in-flight completion request.
// TODO: implement $/cancelRequest when completion is async.
func (m *Manager) CancelCompletion() {}

// --- lang.ReferenceProvider ---

// References finds all references to the symbol at the given position.
func (m *Manager) References(ctx context.Context, path string, line, col int) ([]lang.Location, error) {
	srv, err := m.serverForPath(path)
	if err != nil {
		return nil, err
	}

	params := struct {
		TextDocument lspTextDocumentIdentifier `json:"textDocument"`
		Position     lspPosition               `json:"position"`
		Context      struct {
			IncludeDeclaration bool `json:"includeDeclaration"`
		} `json:"context"`
	}{
		TextDocument: lspTextDocumentIdentifier{URI: pathToURI(path)},
		Position:     srv.toLSPPosition(line, col),
	}
	params.Context.IncludeDeclaration = true

	raw, err := srv.transport.Request(ctx, "textDocument/references", params)
	if err != nil {
		return nil, fmt.Errorf("references request: %w", err)
	}

	var locs []lspLocation
	if err := json.Unmarshal(raw, &locs); err != nil {
		slog.Debug("lsp: references unmarshal failed", "err", err)
		return nil, nil
	}

	results := make([]lang.Location, 0, len(locs))
	for _, loc := range locs {
		l, c := srv.fromLSPPosition(loc.Range.Start)
		results = append(results, lang.Location{
			Path: uriToPath(loc.URI),
			Line: l,
			Col:  c,
		})
	}
	return results, nil
}

// --- lang.SymbolProvider ---

// WorkspaceSymbols queries the language server for symbols matching a query.
func (m *Manager) WorkspaceSymbols(ctx context.Context, query string) ([]lang.SymbolInfo, error) {
	// Use any available server — workspace/symbol is project-wide.
	m.mu.RLock()
	var srv *Server
	for _, s := range m.servers {
		srv = s
		break
	}
	m.mu.RUnlock()
	if srv == nil {
		return nil, fmt.Errorf("no LSP server available")
	}

	params := struct {
		Query string `json:"query"`
	}{Query: query}

	raw, err := srv.transport.Request(ctx, "workspace/symbol", params)
	if err != nil {
		return nil, fmt.Errorf("workspace/symbol request: %w", err)
	}

	var symbols []lspSymbolInformation
	if err := json.Unmarshal(raw, &symbols); err != nil {
		slog.Debug("lsp: workspace/symbol unmarshal failed", "err", err)
		return nil, nil
	}

	results := make([]lang.SymbolInfo, 0, len(symbols))
	for _, sym := range symbols {
		l, c := srv.fromLSPPosition(sym.Location.Range.Start)
		results = append(results, lang.SymbolInfo{
			Name: sym.Name,
			Kind: symbolKindToString(sym.Kind),
			Location: lang.Location{
				Path: uriToPath(sym.Location.URI),
				Line: l,
				Col:  c,
			},
		})
	}
	return results, nil
}

// lspSymbolInformation is the LSP SymbolInformation type.
type lspSymbolInformation struct {
	Name     string      `json:"name"`
	Kind     int         `json:"kind"`
	Location lspLocation `json:"location"`
}

// symbolKindNames maps LSP SymbolKind numbers to human-readable strings.
var symbolKindNames = map[int]string{
	2:  "module",
	3:  "namespace",
	4:  "package",
	5:  "class",
	6:  "method",
	8:  "field",
	9:  "constructor",
	10: "enum",
	11: "interface",
	12: "function",
	13: "variable",
	14: "constant",
	15: "string",
	17: "array",
	19: "object",
	22: "struct",
	23: "event",
	24: "operator",
	25: "type parameter",
	26: "type",
}

// symbolKindToString returns a human-readable name for an LSP SymbolKind.
func symbolKindToString(kind int) string {
	if name, ok := symbolKindNames[kind]; ok {
		return name
	}
	return "symbol"
}

// --- Internal ---

// serverFor returns the server for a language, starting it lazily if needed.
// Returns nil if no config exists for the language.
func (m *Manager) serverFor(languageID string) *Server {
	m.mu.RLock()
	srv, ok := m.servers[languageID]
	m.mu.RUnlock()
	if ok {
		return srv
	}

	// Check if we have a config for this language.
	cfg, ok := m.configs[languageID]
	if !ok {
		return nil
	}

	// Start the server (may take a moment).
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock.
	if srv, ok := m.servers[languageID]; ok {
		return srv
	}

	rootURI := pathToURI(m.projectRoot)
	srv, err := newServer(cfg, rootURI)
	if err != nil {
		slog.Error("lsp: failed to start server", "language", languageID, "command", cfg.Command, "err", err)
		return nil
	}

	// Register diagnostic notification handler.
	srv.OnNotification("textDocument/publishDiagnostics", func(params json.RawMessage) {
		m.handleDiagnostics(srv, params)
	})

	m.servers[languageID] = srv
	slog.Info("lsp: started server", "language", languageID, "command", cfg.Command)
	return srv
}

// serverForPath returns the server for a file's language.
// Uses the stored docLang from DidOpen for consistent routing.
func (m *Manager) serverForPath(path string) (*Server, error) {
	m.mu.RLock()
	languageID := m.docLang[path]
	srv, ok := m.servers[languageID]
	m.mu.RUnlock()
	if languageID == "" || !ok || srv == nil {
		return nil, fmt.Errorf("document not open or no LSP server for %s", path)
	}
	return srv, nil
}

// handleDiagnostics processes a publishDiagnostics notification from a server.
func (m *Manager) handleDiagnostics(srv *Server, params json.RawMessage) {
	var p lspPublishDiagnosticsParams
	if err := json.Unmarshal(params, &p); err != nil {
		slog.Debug("lsp: invalid publishDiagnostics", "err", err)
		return
	}

	path := uriToPath(p.URI)
	diags := make([]lang.Diagnostic, len(p.Diagnostics))
	for i, d := range p.Diagnostics {
		startLine, startCol := srv.fromLSPPosition(d.Range.Start)
		endLine, endCol := srv.fromLSPPosition(d.Range.End)

		code := ""
		if d.Code != nil {
			code = fmt.Sprintf("%v", d.Code)
		}

		diags[i] = lang.Diagnostic{
			StartLine: startLine,
			StartCol:  startCol,
			EndLine:   endLine,
			EndCol:    endCol,
			Severity:  lang.Severity(d.Severity),
			Message:   d.Message,
			Source:    d.Source,
			Code:      code,
		}
	}

	m.mu.Lock()
	m.diags[path] = diags
	m.mu.Unlock()

	// Notify frontend via event channel.
	if m.events != nil {
		select {
		case m.events <- event.DiagnosticsUpdated{Path: path}:
		default:
			slog.Warn("lsp: dropped diagnostics event (channel full)", "path", path)
		}
	}
}
