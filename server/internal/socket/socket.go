package socket

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/latebit/junto/protocol"
)

// Handler is called for each parsed message from a client.
type Handler func(client *Client, msg any)

// Server manages a Unix socket listener and connected clients.
type Server struct {
	listener net.Listener
	sockPath string

	mu      sync.Mutex
	clients map[*Client]struct{}

	handler Handler
}

// Client represents a single connected client.
type Client struct {
	conn net.Conn
	srv  *Server
	mu   sync.Mutex
}

// Send marshals and writes a message to this client.
func (c *Client) Send(msg any) error {
	data, err := protocol.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.conn.Write(data)
	return err
}

// NewServer creates a server listening on a Unix socket with a PID-based session path.
func NewServer(handler Handler) (*Server, error) {
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("junto-%d.sock", os.Getpid()))

	// Clean up stale socket file if it exists.
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}

	log.Printf("listening on %s", sockPath)

	return &Server{
		listener: listener,
		sockPath: sockPath,
		clients:  make(map[*Client]struct{}),
		handler:  handler,
	}, nil
}

// SockPath returns the socket file path for clients to connect to.
func (s *Server) SockPath() string {
	return s.sockPath
}

// Serve accepts connections in a loop. Blocks until the listener is closed.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		client := &Client{conn: conn, srv: s}
		s.addClient(client)
		go s.handleClient(client)
	}
}

// Broadcast sends a message to all connected clients.
func (s *Server) Broadcast(msg any) {
	data, err := protocol.Marshal(msg)
	if err != nil {
		log.Printf("broadcast marshal error: %v", err)
		return
	}
	s.BroadcastRaw(data)
}

// BroadcastRaw sends pre-marshalled JSON bytes to all clients.
func (s *Server) BroadcastRaw(data []byte) {
	s.mu.Lock()
	clients := make([]*Client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		c.mu.Lock()
		_, err := c.conn.Write(data)
		c.mu.Unlock()
		if err != nil {
			log.Printf("broadcast write error: %v", err)
		}
	}
}

// Close shuts down the listener and removes the socket file.
func (s *Server) Close() error {
	err := s.listener.Close()
	os.Remove(s.sockPath)
	return err
}

func (s *Server) addClient(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c] = struct{}{}
	log.Printf("client connected (%d total)", len(s.clients))
}

func (s *Server) removeClient(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
	c.conn.Close()
	log.Printf("client disconnected (%d total)", len(s.clients))
}

func (s *Server) handleClient(c *Client) {
	defer s.removeClient(c)

	// Notify handler of connection (msg=nil signals new client).
	if s.handler != nil {
		s.handler(c, nil)
	}

	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Validate it's JSON before parsing.
		if !json.Valid(line) {
			log.Printf("invalid JSON from client: %s", line)
			continue
		}

		msg, err := protocol.Parse(line)
		if err != nil {
			log.Printf("parse error: %v", err)
			continue
		}

		if s.handler != nil {
			s.handler(c, msg)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("client read error: %v", err)
	}
}
