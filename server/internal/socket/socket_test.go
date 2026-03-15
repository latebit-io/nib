package socket

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/junto/protocol"
)

func startTestServer(t *testing.T, handler Handler) *Server {
	t.Helper()
	srv, err := NewServer(handler)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve() }()
	// No sleep needed — net.Listen already bound the socket in NewServer.
	// Serve() just calls Accept() which the OS queues connections for.
	return srv
}

func dialTestServer(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", srv.SockPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readLine(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		t.Fatalf("readLine: no data (err=%v)", scanner.Err())
	}
	return scanner.Bytes()
}

func writeLine(t *testing.T, conn net.Conn, msg any) {
	t.Helper()
	data, err := protocol.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestServerAcceptsConnection(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := startTestServer(t, func(c *Client, msg any) {
		if _, ok := msg.(ConnectMsg); ok {
			connected <- struct{}{}
		}
	})

	dialTestServer(t, srv)

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection")
	}
}

func TestServerDispatchesMessage(t *testing.T) {
	received := make(chan any, 1)
	connected := make(chan struct{}, 1)
	srv := startTestServer(t, func(c *Client, msg any) {
		if _, ok := msg.(ConnectMsg); ok {
			connected <- struct{}{}
			return
		}
		received <- msg
	})

	conn := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connect callback")
	}

	writeLine(t, conn, protocol.ApproveMsg{Type: protocol.TypeApprove, OpID: "test-1"})

	select {
	case msg := <-received:
		m, ok := msg.(*protocol.ApproveMsg)
		if !ok {
			t.Fatalf("expected *ApproveMsg, got %T", msg)
		}
		if m.OpID != "test-1" {
			t.Errorf("OpID = %q, want %q", m.OpID, "test-1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestClientSend(t *testing.T) {
	srv := startTestServer(t, func(c *Client, msg any) {
		if _, ok := msg.(ConnectMsg); ok {
			// On connect, send a message back to the client.
			_ = c.Send(protocol.TokenMsg{Type: protocol.TypeToken, Text: "hello"})
		}
	})

	conn := dialTestServer(t, srv)
	line := readLine(t, conn)

	var got protocol.TokenMsg
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want %q", got.Text, "hello")
	}
}

func TestBroadcast(t *testing.T) {
	var clients sync.WaitGroup
	clients.Add(2)

	srv := startTestServer(t, func(c *Client, msg any) {
		if _, ok := msg.(ConnectMsg); ok {
			clients.Done()
		}
	})

	conn1 := dialTestServer(t, srv)
	conn2 := dialTestServer(t, srv)
	clients.Wait()

	srv.Broadcast(protocol.TokenMsg{Type: protocol.TypeToken, Text: "broadcast"})

	for i, conn := range []net.Conn{conn1, conn2} {
		line := readLine(t, conn)
		var got protocol.TokenMsg
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("client %d unmarshal: %v", i, err)
		}
		if got.Text != "broadcast" {
			t.Errorf("client %d Text = %q, want %q", i, got.Text, "broadcast")
		}
	}
}

func TestClientDisconnect(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	connected := make(chan struct{}, 1)
	srv := startTestServer(t, func(c *Client, msg any) {
		if _, ok := msg.(ConnectMsg); ok {
			connected <- struct{}{}
		}
	})

	conn := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connect callback")
	}

	// Verify client count.
	srv.mu.Lock()
	count := len(srv.clients)
	srv.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 client, got %d", count)
	}

	_ = conn.Close()

	// Wait for disconnect to propagate.
	go func() {
		for {
			srv.mu.Lock()
			n := len(srv.clients)
			srv.mu.Unlock()
			if n == 0 {
				disconnected <- struct{}{}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("client not removed after disconnect")
	}
}

func TestInvalidJSONIgnored(t *testing.T) {
	received := make(chan any, 1)
	connected := make(chan struct{}, 1)
	srv := startTestServer(t, func(c *Client, msg any) {
		if _, ok := msg.(ConnectMsg); ok {
			connected <- struct{}{}
			return
		}
		received <- msg
	})

	conn := dialTestServer(t, srv)
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connect callback")
	}

	// Send invalid JSON, then valid JSON.
	_, _ = conn.Write([]byte("not json\n"))
	writeLine(t, conn, protocol.ApproveMsg{Type: protocol.TypeApprove, OpID: "after-bad"})

	select {
	case msg := <-received:
		m := msg.(*protocol.ApproveMsg)
		if m.OpID != "after-bad" {
			t.Errorf("OpID = %q, want %q", m.OpID, "after-bad")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out — invalid JSON may have broken the read loop")
	}
}
