package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
)

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

	// socket → stdout
	go func() {
		if _, err := io.Copy(os.Stdout, conn); err != nil {
			log.Printf("socket→stdout: %v", err)
		}
		os.Exit(0)
	}()

	// stdin → socket
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Re-append newline since scanner strips it.
		line = append(line, '\n')
		if _, err := conn.Write(line); err != nil {
			log.Fatalf("stdin→socket: %v", err)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("stdin read: %v", err)
	}
}
