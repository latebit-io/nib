package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// mockServer simulates an LSP server: reads requests, sends responses.
type mockServer struct {
	clientReader io.Reader      // reads what the client writes (client's stdin→server)
	clientWriter io.WriteCloser // writes to what the client reads (server→client's stdout)
}

// readRequest reads one Content-Length framed JSON-RPC message from the client.
func (s *mockServer) readRequest() (json.RawMessage, error) {
	var contentLength int
	buf := make([]byte, 1)
	var headerBuf strings.Builder
	for {
		_, err := s.clientReader.Read(buf)
		if err != nil {
			return nil, err
		}
		headerBuf.WriteByte(buf[0])
		if strings.HasSuffix(headerBuf.String(), "\r\n\r\n") {
			break
		}
	}
	header := headerBuf.String()
	_, err := fmt.Sscanf(header, "Content-Length: %d\r\n\r\n", &contentLength)
	if err != nil {
		return nil, fmt.Errorf("parse header %q: %w", header, err)
	}
	body := make([]byte, contentLength)
	_, err = io.ReadFull(s.clientReader, body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// sendResponse sends a Content-Length framed JSON-RPC response.
func (s *mockServer) sendResponse(body []byte) error {
	framed := frame(body)
	_, err := s.clientWriter.Write(framed)
	return err
}

// newMockPair creates a Transport connected to a mock server via pipes.
func newMockPair() (*Transport, *mockServer) {
	// Client writes → server reads
	clientToServerR, clientToServerW := io.Pipe()
	// Server writes → client reads
	serverToClientR, serverToClientW := io.Pipe()

	transport := NewTransport(serverToClientR, clientToServerW)
	server := &mockServer{
		clientReader: clientToServerR,
		clientWriter: serverToClientW,
	}
	return transport, server
}

func TestTransportRequestResponse(t *testing.T) {
	tr, server := newMockPair()
	defer func() { _ = tr.Close() }()

	// Server goroutine: read request, send response.
	go func() {
		body, err := server.readRequest()
		if err != nil {
			return
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return
		}
		resp, _ := json.Marshal(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      &req.ID,
			Result:  json.RawMessage(`{"capabilities":{}}`),
		})
		if err := server.sendResponse(resp); err != nil {
			return
		}
	}()

	result, err := tr.Request(context.Background(), "initialize", map[string]any{"processId": 1})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if string(result) != `{"capabilities":{}}` {
		t.Errorf("got result %s, want {\"capabilities\":{}}", string(result))
	}
}

func TestTransportNotification(t *testing.T) {
	tr, server := newMockPair()
	defer func() { _ = tr.Close() }()

	// Server goroutine: read notification.
	done := make(chan json.RawMessage, 1)
	go func() {
		body, err := server.readRequest()
		if err != nil {
			return
		}
		done <- body
	}()

	err := tr.Notify("initialized", struct{}{})
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	body := <-done
	var notif jsonRPCNotification
	if err := json.Unmarshal(body, &notif); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if notif.Method != "initialized" {
		t.Errorf("got method %q, want %q", notif.Method, "initialized")
	}
}

func TestTransportServerNotification(t *testing.T) {
	tr, server := newMockPair()
	defer func() { _ = tr.Close() }()

	received := make(chan string, 1)
	tr.OnNotification("textDocument/publishDiagnostics", func(params json.RawMessage) {
		received <- string(params)
	})

	// Server sends a notification.
	notif, _ := json.Marshal(jsonRPCNotification{
		JSONRPC: "2.0",
		Method:  "textDocument/publishDiagnostics",
		Params:  json.RawMessage(`{"uri":"file:///test.go"}`),
	})
	if err := server.sendResponse(notif); err != nil {
		t.Fatalf("sendResponse failed: %v", err)
	}

	got := <-received
	if got != `{"uri":"file:///test.go"}` {
		t.Errorf("got params %s, want {\"uri\":\"file:///test.go\"}", got)
	}
}

func TestTransportConcurrentRequests(t *testing.T) {
	tr, server := newMockPair()
	defer func() { _ = tr.Close() }()

	// Server goroutine: read N requests, send responses (possibly out of order).
	const n = 10
	go func() {
		for range n {
			body, err := server.readRequest()
			if err != nil {
				return
			}
			var req jsonRPCRequest
			if err := json.Unmarshal(body, &req); err != nil {
				return
			}
			resp, _ := json.Marshal(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      &req.ID,
				Result:  json.RawMessage(fmt.Sprintf(`{"id":%d}`, req.ID)),
			})
			if err := server.sendResponse(resp); err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := tr.Request(context.Background(), "test", map[string]int{"i": i})
			if err != nil {
				t.Errorf("request %d failed: %v", i, err)
				return
			}
			var got struct{ ID int }
			if err := json.Unmarshal(result, &got); err != nil {
				t.Errorf("unmarshal result for request %d: %v", i, err)
				return
			}
			// We can't guarantee which request gets which response in this simple mock,
			// but all requests should complete without error.
			_ = got
		}()
	}
	wg.Wait()
}

func TestTransportCloseUnblocksPending(t *testing.T) {
	tr, _ := newMockPair()

	// Start a request that will never get a response.
	done := make(chan error, 1)
	go func() {
		_, err := tr.Request(context.Background(), "test", nil)
		done <- err
	}()

	// Close the transport — should unblock the pending request.
	_ = tr.Close()

	err := <-done
	if err == nil {
		t.Error("expected error from closed transport, got nil")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"test"}`)
	framed := frame(body)
	expected := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	if string(framed) != expected {
		t.Errorf("frame produced %q, want %q", string(framed), expected)
	}
}
