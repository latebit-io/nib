package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/kit"
)

// handshakeTimeout bounds one server's start, handshake and tool
// listing, so a server that hangs cannot hold up the session.
const handshakeTimeout = 10 * time.Second

// Session is a set of running servers and their tools. Close it when
// the agent is done.
type Session struct {
	// Tools are every connected server's tools, in server-name order.
	Tools []agent.Tool
	// Connected are the servers that started.
	Connected []Connected
	// Failed are the servers that did not start, or tools left out.
	Failed []Failure

	clients []*Client
}

// Connected is one running server and its tools.
type Connected struct {
	Server Server
	Tools  []agent.Tool
}

// Failure is a server that did not start, or one of its tools that
// could not be offered.
type Failure struct {
	Server Server
	Err    error
}

// Connect starts servers at the same time and lists their tools. A
// server that fails is recorded in [Session.Failed]; the rest still
// run. The caller owns the returned Session and must Close it.
func Connect(ctx context.Context, servers []Server) *Session {
	type outcome struct {
		client *Client
		tools  []agent.Tool
		errs   []error
	}
	outcomes := make([]outcome, len(servers))
	var wg sync.WaitGroup
	for i, server := range servers {
		wg.Go(func() {
			client, tools, errs := start(ctx, server)
			outcomes[i] = outcome{client, tools, errs}
		})
	}
	wg.Wait()

	s := &Session{}
	for i, o := range outcomes {
		for _, err := range o.errs {
			s.Failed = append(s.Failed, Failure{Server: servers[i], Err: err})
		}
		if o.client == nil {
			continue
		}
		s.clients = append(s.clients, o.client)
		s.Connected = append(s.Connected, Connected{Server: servers[i], Tools: o.tools})
		s.Tools = append(s.Tools, o.tools...)
	}
	return s
}

// start runs one server through its handshake. client is nil when the
// server could not be used; errs also carries tools that were left out.
func start(ctx context.Context, server Server) (client *Client, tools []agent.Tool, errs []error) {
	fields := strings.Fields(server.Command)
	if len(fields) == 0 {
		return nil, nil, []error{errors.New("no command")}
	}
	client, err := NewStdioClient(fields[0], slices.Concat(fields[1:], server.Args), environ(server.Env))
	if err != nil {
		return nil, nil, []error{err}
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	infos, err := handshake(ctx, client)
	if err != nil {
		// The handshake error is the one to report; Close only reaps the process.
		_ = client.Close()
		return nil, nil, []error{err}
	}
	for _, info := range infos {
		t, err := newTool(client, server.Name, info)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tools = append(tools, t)
	}
	return client, tools, errs
}

// handshake initializes the connection and lists the server's tools.
func handshake(ctx context.Context, client *Client) ([]ToolInfo, error) {
	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	infos, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	return infos, nil
}

// environ returns the inherited environment with extra appended, which
// wins: a process takes the last value of a repeated key. Nil without
// extra, which means "inherit".
func environ(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	for _, key := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, key+"="+extra[key])
	}
	return env
}

// Close stops every server, all at once: one that is slow to exit
// must not hold up the rest. Safe on a nil Session.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	errs := make([]error, len(s.clients))
	var wg sync.WaitGroup
	for i, c := range s.clients {
		wg.Go(func() { errs[i] = c.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Plugins lists the session for a `--plugins` manifest: each server's
// tools, each server that failed, and each definition cfg refused, so
// nothing a config names goes missing without a reason.
func Plugins(cfg Config, s *Session) []kit.Plugin {
	var out []kit.Plugin
	if s != nil {
		for _, c := range s.Connected {
			for _, t := range c.Tools {
				p := kit.DescribeTool(t)
				p.Source = "mcp:" + c.Server.Name
				out = append(out, p)
			}
		}
		for _, f := range s.Failed {
			out = append(out, serverEntry(f.Server, "failed: "+f.Err.Error()))
		}
	}
	for _, r := range cfg.Refused {
		out = append(out, serverEntry(r.Server, "refused: "+r.Err.Error()))
	}
	return out
}

// serverEntry is the manifest line for a server that offers no tools.
func serverEntry(s Server, status string) kit.Plugin {
	return kit.Plugin{
		Kind: kit.KindTool, Name: ToolNamePrefix + sanitize(s.Name), Description: status,
		Source: string(s.Origin) + ":" + s.Source,
	}
}
