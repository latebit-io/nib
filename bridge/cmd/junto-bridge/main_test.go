package main

import (
	"bufio"
	"io"
	"net"
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
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					_, _ = c.Write(scanner.Bytes())
					_, _ = c.Write([]byte{'\n'})
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
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte(greeting))
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					_, _ = c.Write(scanner.Bytes())
					_, _ = c.Write([]byte{'\n'})
				}
			}(conn)
		}
	}()

	return sockPath, ln
}

// readLine reads one line from a PipeReader with a timeout.
func readLine(t *testing.T, r *io.PipeReader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			ch <- result{line: scanner.Text()}
		} else {
			ch <- result{err: scanner.Err()}
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("readLine error: %v", res.err)
		}
		return res.line
	case <-time.After(2 * time.Second):
		t.Fatal("readLine timed out")
		return ""
	}
}

func TestRunRelaysStdinToSocket(t *testing.T) {
	sockPath, _ := startTestSocket(t)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	msg := `{"type":"approve","op_id":"test"}`
	pr, pw := io.Pipe()
	outR, outW := io.Pipe()

	go run(pr, outW, conn)

	_, _ = pw.Write([]byte(msg + "\n"))

	got := readLine(t, outR)
	if got != msg {
		t.Errorf("echoed = %q, want %q", got, msg)
	}

	_ = pw.Close()
}

func TestRunRelaysSocketToStdout(t *testing.T) {
	greeting := `{"type":"token","text":"hello from server"}` + "\n"
	sockPath, _ := startTestSocketWithGreeting(t, greeting)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	pr, pw := io.Pipe()
	outR, outW := io.Pipe()

	go run(pr, outW, conn)

	got := readLine(t, outR)
	want := strings.TrimSpace(greeting)
	if got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}

	_ = pw.Close()
}

func TestRunExitsOnStdinClose(t *testing.T) {
	sockPath, _ := startTestSocket(t)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	pr, pw := io.Pipe()
	_, outW := io.Pipe()

	done := make(chan struct{})
	go func() {
		run(pr, outW, conn)
		close(done)
	}()

	_, _ = pw.Write([]byte("hello\n"))
	_ = pw.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after stdin close")
	}
}
