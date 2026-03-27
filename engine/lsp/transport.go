// Package lsp implements the LSP client adapter.
// It communicates with language servers via JSON-RPC 2.0 over stdio
// using Content-Length framing (LSP base protocol).
package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// outboxSize is the capacity of the async write queue.
// Large enough to absorb bursts without blocking the caller.
const outboxSize = 256

// Transport implements JSON-RPC 2.0 over Content-Length framed stdio.
// Writes are async (non-blocking Send). Reads are dispatched in a background
// goroutine that routes responses to pending requests and notifications to
// registered callbacks.
type Transport struct {
	rawReader io.Reader // underlying reader, stored for Close
	writer    io.WriteCloser
	reader    *bufio.Reader

	outbox chan []byte   // async write queue
	done   chan struct{} // closed when readLoop exits

	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage // request ID → response channel

	// notifyHandlers maps method → callback for server-initiated notifications.
	notifyMu sync.RWMutex
	notify   map[string]func(json.RawMessage)

	closeOnce sync.Once
	closed    chan struct{}
}

// NewTransport creates a transport over the given reader/writer pair.
// Starts background goroutines for reading and writing. Call Close to stop.
func NewTransport(r io.Reader, w io.WriteCloser) *Transport {
	t := &Transport{
		rawReader: r,
		writer:    w,
		reader:    bufio.NewReaderSize(r, 64*1024),
		outbox:    make(chan []byte, outboxSize),
		done:      make(chan struct{}),
		pending:   make(map[int]chan json.RawMessage),
		notify:    make(map[string]func(json.RawMessage)),
		closed:    make(chan struct{}),
	}
	go t.readLoop()
	go t.writeLoop()
	return t
}

// --- JSON-RPC message types ---

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// incomingMessage is used to peek at an incoming JSON-RPC message
// to determine whether it's a response (has ID) or notification (has method).
type incomingMessage struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *jsonRPCError   `json:"error"`
}

// --- Public API ---

// Request sends a JSON-RPC request and blocks until a response is received.
// Returns the result payload or an error. Thread-safe.
func (t *Transport) Request(method string, params any) (json.RawMessage, error) {
	paramBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	t.mu.Lock()
	id := t.nextID
	t.nextID++
	ch := make(chan json.RawMessage, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	msg := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  paramBytes,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		t.removePending(id)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if err := t.send(body); err != nil {
		t.removePending(id)
		return nil, err
	}

	// Wait for response or transport close.
	select {
	case result, ok := <-ch:
		if !ok {
			return nil, errors.New("transport closed before response received")
		}
		return result, nil
	case <-t.closed:
		t.removePending(id)
		return nil, errors.New("transport closed before response received")
	}
}

// Notify sends a JSON-RPC notification (no response expected). Non-blocking.
func (t *Transport) Notify(method string, params any) error {
	paramBytes, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}

	msg := jsonRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramBytes,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	return t.send(body)
}

// OnNotification registers a callback for server-initiated notifications.
// The callback receives the raw params JSON. Thread-safe.
func (t *Transport) OnNotification(method string, handler func(json.RawMessage)) {
	t.notifyMu.Lock()
	t.notify[method] = handler
	t.notifyMu.Unlock()
}

// Close shuts down the transport. Drains pending requests with errors.
func (t *Transport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		// Close writer to signal EOF to the server.
		_ = t.writer.Close() // best-effort during shutdown
		// Close reader to unblock readLoop. In a real LSP server, killing
		// the process closes stdout. With pipes (tests), we must close explicitly.
		if rc, ok := t.rawReader.(io.Closer); ok {
			_ = rc.Close() // best-effort during shutdown
		}
	})
	// Wait for readLoop to finish (it exits on reader EOF or error).
	<-t.done
	return nil
}

// --- Internal ---

// send enqueues a framed message to the async write queue. Non-blocking.
func (t *Transport) send(body []byte) error {
	framed := frame(body)
	select {
	case t.outbox <- framed:
		return nil
	case <-t.closed:
		return errors.New("transport closed")
	default:
		return errors.New("transport write queue full")
	}
}

// frame wraps a JSON body in Content-Length headers per LSP base protocol.
func frame(body []byte) []byte {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	result := make([]byte, len(header)+len(body))
	copy(result, header)
	copy(result[len(header):], body)
	return result
}

// writeLoop drains the outbox and writes to the server's stdin.
// Runs in a goroutine. Exits when outbox is closed or transport is closed.
func (t *Transport) writeLoop() {
	for {
		select {
		case msg, ok := <-t.outbox:
			if !ok {
				return
			}
			if _, err := t.writer.Write(msg); err != nil {
				slog.Debug("lsp transport: write error", "err", err)
				return
			}
		case <-t.closed:
			return
		}
	}
}

// readLoop reads Content-Length framed messages from the server's stdout.
// Dispatches responses to pending requests and notifications to handlers.
// Runs in a goroutine. Exits on EOF or read error.
func (t *Transport) readLoop() {
	defer close(t.done)
	defer t.drainPending()

	for {
		body, err := t.readMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				// Check if transport was intentionally closed.
				select {
				case <-t.closed:
					return
				default:
				}
				slog.Debug("lsp transport: read error", "err", err)
			}
			return
		}

		var msg incomingMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			slog.Debug("lsp transport: invalid JSON from server", "err", err)
			continue
		}

		if msg.ID != nil {
			// Response to a request we sent.
			t.handleResponse(&msg)
		} else if msg.Method != "" {
			// Server-initiated notification.
			t.handleNotification(msg.Method, msg.Params)
		}
	}
}

// readMessage reads one Content-Length framed message from the reader.
func (t *Transport) readMessage() ([]byte, error) {
	// Read headers until empty line.
	contentLength := -1
	for {
		line, err := t.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if strings.HasPrefix(line, "Content-Length:") {
			valStr := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			n, err := strconv.Atoi(valStr)
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", valStr, err)
			}
			contentLength = n
		}
		// Content-Type and other headers are ignored per LSP spec.
	}

	if contentLength < 0 {
		return nil, errors.New("missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(t.reader, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// handleResponse routes a response to the pending request channel.
func (t *Transport) handleResponse(msg *incomingMessage) {
	t.mu.Lock()
	ch, ok := t.pending[*msg.ID]
	if ok {
		delete(t.pending, *msg.ID)
	}
	t.mu.Unlock()

	if !ok {
		slog.Debug("lsp transport: response for unknown request", "id", *msg.ID)
		return
	}

	if msg.Error != nil {
		// Encode the error as a JSON result so the caller can inspect it.
		errJSON, _ := json.Marshal(msg.Error)
		ch <- errJSON
		close(ch)
		return
	}

	ch <- msg.Result
	close(ch)
}

// handleNotification invokes the registered callback for a notification.
func (t *Transport) handleNotification(method string, params json.RawMessage) {
	t.notifyMu.RLock()
	handler, ok := t.notify[method]
	t.notifyMu.RUnlock()
	if ok {
		handler(params)
	}
}

// removePending removes a pending request channel (e.g., on send failure).
func (t *Transport) removePending(id int) {
	t.mu.Lock()
	ch, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
		close(ch)
	}
	t.mu.Unlock()
}

// drainPending closes all pending request channels so blocked callers unblock.
func (t *Transport) drainPending() {
	t.mu.Lock()
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
	t.mu.Unlock()
}
