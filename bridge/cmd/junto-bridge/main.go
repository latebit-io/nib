package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
)

// run relays data between in/out and a network connection.
// It copies conn→out in a goroutine and scans in→conn line-by-line.
// Returns when in is closed/EOF or conn is closed.
func run(in io.Reader, out io.Writer, conn net.Conn) {
	// socket → out
	go func() {
		if _, err := io.Copy(out, conn); err != nil {
			log.Printf("socket→stdout: %v", err)
		}
		// Close conn to unblock the in→socket scanner, allowing run to return.
		conn.Close()
	}()

	// in → socket
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Re-append newline since scanner strips it.
		line = append(line, '\n')
		if _, err := conn.Write(line); err != nil {
			log.Printf("in→socket: %v", err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("in read: %v", err)
	}
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
	defer conn.Close()

	run(os.Stdin, os.Stdout, conn)
}
