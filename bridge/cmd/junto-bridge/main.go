package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
)

// run relays data between in/out and a network connection.
// It copies conn→out in a goroutine and scans in→conn line-by-line.
// Returns when in is closed/EOF or conn is closed.
func run(in io.Reader, out io.Writer, conn net.Conn) {
	var wg sync.WaitGroup

	// socket → out
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(out, conn); err != nil {
			log.Printf("socket→stdout: %v", err)
		}
	}()

	// in → socket
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy and re-append newline (scanner.Bytes() shares internal buffer).
		msg := make([]byte, len(line)+1)
		copy(msg, line)
		msg[len(line)] = '\n'
		if _, err := conn.Write(msg); err != nil {
			log.Printf("in→socket: %v", err)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("in read: %v", err)
	}

	// Close conn to unblock the socket→out goroutine, then wait for it.
	_ = conn.Close()
	wg.Wait()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: junto-bridge <socket-path>\n")
		os.Exit(1)
	}
	sockPath := os.Args[1]

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Fatalf("connect to %s: %v", sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	run(os.Stdin, os.Stdout, conn)
}
