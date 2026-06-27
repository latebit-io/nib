package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/latebit-io/nib/engine/lang"
)

// Server manages the lifecycle of a single LSP server process.
// It handles initialization, document sync, and shutdown.
type Server struct {
	cmd       *exec.Cmd
	transport *Transport

	// posEncoding is the negotiated position encoding ("utf-32" or "utf-16").
	posEncoding string

	// capabilities from the server's initialize response.
	capabilities lspServerCapabilities

	mu          sync.Mutex
	initialized bool
	rootURI     string
}

// ServerConfig defines how to launch a language server.
// Injected at construction — the lsp package never hardcodes server binaries.
type ServerConfig struct {
	// Command is the language server executable (e.g., "gopls").
	Command string
	// Args are the command-line arguments (e.g., ["serve"]).
	Args []string
	// Env is optional extra environment variables for the server process.
	Env []string
	// LanguageID is the language identifier (e.g., "go") matching lang.DetectLanguage.
	LanguageID string
}

// newServer spawns a language server process and initializes it.
// Returns a ready-to-use Server or an error.
func newServer(cfg ServerConfig, rootURI string) (*Server, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append(os.Environ(), cfg.Env...)
	// Discard stderr to avoid corrupting TUI alt-screen.
	// Server errors are reported via JSON-RPC error responses.
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.Command, err)
	}

	transport := NewTransport(stdout, stdin)

	s := &Server{
		cmd:       cmd,
		transport: transport,
		rootURI:   rootURI,
	}

	if err := s.initialize(); err != nil {
		_ = s.Close() // best-effort cleanup; initialize error takes priority
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return s, nil
}

// initialize performs the LSP initialize/initialized handshake.
func (s *Server) initialize() error {
	params := lspInitializeParams{
		ProcessID: os.Getpid(),
		RootURI:   s.rootURI,
		Capabilities: lspClientCapabilities{
			General: &lspGeneralCapabilities{
				// Request UTF-32 (rune-based, zero conversion cost).
				// Fall back to UTF-16 if server doesn't support it.
				PositionEncodings: []string{positionEncodingUTF32, positionEncodingUTF16},
			},
			TextDocument: &lspTextDocumentClientCapabilities{
				Synchronization: &lspTextDocumentSyncClientCapabilities{
					DidSave: true,
				},
				Completion:         &lspCompletionClientCapabilities{},
				Hover:              &lspHoverClientCapabilities{},
				PublishDiagnostics: &lspPublishDiagnosticsClientCapabilities{},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := s.transport.Request(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize request: %w", err)
	}
	var result lspInitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("unmarshal initialize result: %w", err)
	}

	s.capabilities = result.Capabilities

	// Determine position encoding. Server responds with what it chose.
	s.posEncoding = positionEncodingUTF16 // default per LSP spec
	serverEncoding := result.Capabilities.PositionEncoding
	slog.Debug("lsp server: initialize response", "positionEncoding", serverEncoding)
	if serverEncoding == positionEncodingUTF32 {
		s.posEncoding = positionEncodingUTF32
		slog.Debug("lsp server: negotiated UTF-32 position encoding")
	} else {
		slog.Debug("lsp server: using UTF-16 position encoding (default)",
			"serverReported", serverEncoding)
	}

	// Send initialized notification (no params).
	if err := s.transport.Notify("initialized", struct{}{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}

	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()

	slog.Debug("lsp server: initialized", "encoding", s.posEncoding)
	return nil
}

// OnNotification registers a handler for server-initiated notifications.
func (s *Server) OnNotification(method string, handler func(json.RawMessage)) {
	s.transport.OnNotification(method, handler)
}

// --- Document Sync ---

// DidOpen sends a textDocument/didOpen notification.
func (s *Server) DidOpen(uri, languageID string, version int, content string) {
	if err := s.transport.Notify("textDocument/didOpen", lspDidOpenParams{
		TextDocument: lspTextDocumentItem{
			URI:        uri,
			LanguageID: languageID,
			Version:    version,
			Text:       content,
		},
	}); err != nil {
		slog.Warn("lsp server: didOpen notify failed", "uri", uri, "err", err)
	}
}

// DidChange sends a textDocument/didChange notification with incremental changes.
func (s *Server) DidChange(uri string, version int, changes []lang.TextChange) {
	lspChanges := make([]lspContentChangeEvent, len(changes))
	for i, c := range changes {
		if c.FullContent {
			// Full content sync — no range, just the entire text.
			lspChanges[i] = lspContentChangeEvent{Text: c.Text}
		} else {
			lspChanges[i] = lspContentChangeEvent{
				Range: &lspRange{
					Start: s.toLSPPosition(c.StartLine, c.StartCol),
					End:   s.toLSPPosition(c.EndLine, c.EndCol),
				},
				Text: c.Text,
			}
		}
	}
	if err := s.transport.Notify("textDocument/didChange", lspDidChangeParams{
		TextDocument: lspVersionedTextDocumentIdentifier{
			URI:     uri,
			Version: version,
		},
		ContentChanges: lspChanges,
	}); err != nil {
		slog.Warn("lsp server: didChange notify failed", "uri", uri, "err", err)
	}
}

// DidSave sends a textDocument/didSave notification.
func (s *Server) DidSave(uri string) {
	if err := s.transport.Notify("textDocument/didSave", lspDidSaveParams{
		TextDocument: lspTextDocumentIdentifier{URI: uri},
	}); err != nil {
		slog.Warn("lsp server: didSave notify failed", "uri", uri, "err", err)
	}
}

// DidClose sends a textDocument/didClose notification.
func (s *Server) DidClose(uri string) {
	if err := s.transport.Notify("textDocument/didClose", lspDidCloseParams{
		TextDocument: lspTextDocumentIdentifier{URI: uri},
	}); err != nil {
		slog.Warn("lsp server: didClose notify failed", "uri", uri, "err", err)
	}
}

// --- Position Encoding ---

// toLSPPosition converts rune-based (0-indexed) coordinates to LSP position.
// When UTF-32 is negotiated, this is a no-op (rune == UTF-32 code unit).
//
// LIMITATION: in UTF-16 mode the rune column is passed through unchanged. This
// is correct for all BMP characters but WRONG for off-BMP characters (code
// points > U+FFFF, e.g. many emoji), which occupy two UTF-16 code units — the
// reported character offset will be short by one per preceding off-BMP rune.
// No conversion is attempted here. Mitigation: when UTF-32 is unavailable
// Session uses full-content sync (FullContentSyncer), so DidChange ranges with
// rune positions are never sent to a UTF-16 server; this path is only used for
// request/response methods (definition, hover, completion) whose positions sit
// inside source identifiers, which are BMP-only for the languages we target.
func (s *Server) toLSPPosition(line, col int) lspPosition {
	if s.posEncoding == positionEncodingUTF32 {
		return lspPosition{Line: line, Character: col}
	}
	// UTF-16 fallback: col is in runes, but LSP wants UTF-16 code units.
	// Characters outside BMP (> U+FFFF) take 2 UTF-16 code units (surrogate pair).
	// Pass through — correct for all BMP characters. When UTF-32 is unavailable,
	// Session uses full-content sync (via FullContentSyncer) so DidChange ranges
	// with rune positions are not sent to UTF-16 servers. This path is only used
	// for request/response methods (definition, hover, completion) where positions
	// are within source code identifiers (always BMP for Go).
	return lspPosition{Line: line, Character: col}
}

// fromLSPPosition converts an LSP position to rune-based coordinates.
//
// LIMITATION: the inverse of toLSPPosition. In UTF-16 mode the character offset
// is passed through unchanged — correct for BMP characters but WRONG for lines
// containing off-BMP characters (the rune column will be over-counted by one
// per preceding off-BMP rune). This affects the diagnostic-range path
// (handleDiagnostics), which is acceptable because diagnostic ranges from the
// servers we target land on BMP-only source identifiers. No UTF-16 conversion
// is attempted.
func (s *Server) fromLSPPosition(pos lspPosition) (line, col int) {
	if s.posEncoding == positionEncodingUTF32 {
		return pos.Line, pos.Character
	}
	// UTF-16 fallback: pass through. Correct for BMP characters.
	// Diagnostic/hover/definition positions in Go source code are always BMP.
	return pos.Line, pos.Character
}

// --- Lifecycle ---

// Close shuts down the server process gracefully.
func (s *Server) Close() error {
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()

	if initialized {
		// Try graceful shutdown: send shutdown request, then exit notification.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := s.transport.Request(ctx, "shutdown", nil)
		if err != nil {
			slog.Debug("lsp server: shutdown request failed", "err", err)
		} else {
			// Send exit notification after shutdown response.
			_ = s.transport.Notify("exit", nil) // best-effort; transport closing next
		}
	}

	// Close transport (closes stdin, unblocks readLoop).
	_ = s.transport.Close() // best-effort during shutdown

	// Kill the process if it's still running.
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill() // process may already be dead
	}
	_ = s.cmd.Wait() // reap zombie; error expected after kill
	return nil
}
