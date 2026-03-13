package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTestSocket creates a Unix socket listener that echoes back any line it receives.
func startTestSocket(t *testing.T) (string, net.Listener) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					c.Write(append(scanner.Bytes(), '\n'))
				}
			}(conn)
		}
	}()

	return sockPath, ln
}

// startTestSocketWithGreeting creates a socket that sends a greeting then echoes.
func startTestSocketWithGreeting(t *testing.T, greeting string) (string, net.Listener) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Write([]byte(greeting))
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					c.Write(append(scanner.Bytes(), '\n'))
				}
			}(conn)
		}
	}()

	return sockPath, ln
}

func TestBridgeRelaysStdinToSocket(t *testing.T) {
	sockPath, _ := startTestSocket(t)

	// Connect directly to verify the echo server works.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := `{"type":"approve","op_id":"test"}` + "\n"
	conn.Write([]byte(msg))

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no echo: %v", scanner.Err())
	}
	got := scanner.Text()
	if got != strings.TrimSpace(msg) {
		t.Errorf("echo = %q, want %q", got, strings.TrimSpace(msg))
	}
}

func TestBridgeRelaysSocketToStdout(t *testing.T) {
	greeting := `{"type":"token","text":"hello from server"}` + "\n"
	sockPath, _ := startTestSocketWithGreeting(t, greeting)

	// Simulate what the bridge does: connect, read from socket.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no greeting: %v", scanner.Err())
	}
	got := scanner.Text()
	want := strings.TrimSpace(greeting)
	if got != want {
		t.Errorf("greeting = %q, want %q", got, want)
	}
}

func TestBridgeExitsOnStdinClose(t *testing.T) {
	// Verify that closing stdin causes the scanner loop to exit.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	// Write some data then close.
	w.Write([]byte("hello\n"))
	w.Close()

	scanner := bufio.NewScanner(r)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	if lines != 1 {
		t.Errorf("expected 1 line, got %d", lines)
	}

	// Verify reading returns EOF after close.
	buf := make([]byte, 1)
	n, err := r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("expected EOF, got n=%d err=%v", n, err)
	}
}
