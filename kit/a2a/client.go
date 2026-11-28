package a2a

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/internal/httporigin"
	"github.com/latebit-io/nib/kit/internal/termsafe"
)

// connectTimeout bounds fetching one agent's card.
const connectTimeout = 10 * time.Second

// maxResponseBytes caps every response from a remote agent: the SDK
// reads bodies whole, and a card or an answer is far smaller.
const maxResponseBytes = 8 << 20

// Session is the person's agents, connected for one nib session. Each
// gets a tool; Close the session when done.
type Session struct {
	// Refused are the agents the config left out; Failed, those that
	// could not be connected. Each carries its reason.
	Refused, Failed []Refusal
	agents          []*remote
}

// Connect fetches the card of every agent in cfg at once, each within
// ten seconds. An agent that fails is recorded; the rest connect.
func Connect(ctx context.Context, cfg Config) *Session {
	s := &Session{Refused: cfg.Refused}
	results := make([]*remote, len(cfg.Agents))
	errs := make([]error, len(cfg.Agents))
	var wg sync.WaitGroup
	for i, a := range cfg.Agents {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			results[i], errs[i] = connect(ctx, a)
		})
	}
	wg.Wait()
	for i, r := range results {
		if errs[i] != nil {
			s.Failed = append(s.Failed, Refusal{Agent: cfg.Agents[i], Err: errs[i]})
			continue
		}
		s.agents = append(s.agents, r)
	}
	return s
}

// Tools returns a tool per connected agent, in the order they were named.
func (s *Session) Tools() []kit.Tool {
	if s == nil {
		return nil
	}
	tools := make([]kit.Tool, 0, len(s.agents))
	for _, r := range s.agents {
		tools = append(tools, r)
	}
	return tools
}

// Close releases every agent's client. A second call does nothing.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, r := range s.agents {
		if err := r.client.Destroy(); err != nil {
			errs = append(errs, fmt.Errorf("a2a: %s: %w", r.agent.Name, err))
		}
	}
	s.agents = nil
	return errors.Join(errs...)
}

// Plugins returns the manifest rows: a tool per connected agent, and a
// line for each agent that failed or was refused.
func Plugins(s *Session) []kit.Plugin {
	if s == nil {
		return nil
	}
	var out []kit.Plugin
	for _, r := range s.agents {
		p := kit.DescribeTool(r)
		p.Source = "a2a:" + r.agent.Source
		out = append(out, p)
	}
	for _, f := range s.Failed {
		out = append(out, agentEntry(f.Agent, "failed: "+f.Err.Error()))
	}
	for _, r := range s.Refused {
		out = append(out, agentEntry(r.Agent, "refused: "+r.Err.Error()))
	}
	return out
}

// agentEntry is the manifest line for an agent that offers no tool.
func agentEntry(a Agent, status string) kit.Plugin {
	return kit.Plugin{Kind: kit.KindTool, Name: ToolNamePrefix + a.Name, Description: status, Source: "a2a:" + a.Source}
}

// connect fetches a's card and opens a client on an interface it offers
// on a's own origin: the configured headers are for that origin alone,
// and a message must not leave in the clear.
func connect(ctx context.Context, a Agent) (*remote, error) {
	if err := httporigin.CheckEndpoint(a.URL); err != nil {
		return nil, err
	}
	own := httporigin.Of(a.URL)
	client := httporigin.Client(a.URL, a.Headers)
	client.Transport = &confined{origin: own, base: client.Transport}
	card, err := agentcard.NewResolver(client).Resolve(ctx, a.URL)
	if err != nil {
		return nil, fmt.Errorf("a2a: card: %w", err)
	}
	card.SupportedInterfaces = slices.DeleteFunc(card.SupportedInterfaces, func(i *a2a.AgentInterface) bool {
		return i == nil || httporigin.Of(i.URL) != own
	})
	if len(card.SupportedInterfaces) == 0 {
		return nil, fmt.Errorf("the card offers no interface on %s", own)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(client), a2aclient.WithRESTTransport(client))
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	return newRemote(a, card, c), nil
}

// confined sends requests to one origin only and caps each response.
type confined struct {
	origin string
	base   http.RoundTripper // nil is http.DefaultTransport
}

// RoundTrip implements [http.RoundTripper].
func (t *confined) RoundTrip(req *http.Request) (*http.Response, error) {
	if got := httporigin.Of(req.URL.String()); got != t.origin {
		return nil, fmt.Errorf("a2a: refusing a request to %s: the agent is at %s", got, t.origin)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &capped{ReadCloser: resp.Body, left: maxResponseBytes}
	return resp, nil
}

// capped fails a read past its limit instead of buffering without end.
type capped struct {
	io.ReadCloser
	left int64
}

// Read implements [io.Reader].
func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("a2a: a response over %d bytes", maxResponseBytes)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.ReadCloser.Read(p)
	c.left -= int64(n)
	return n, err
}

// clean turns text from a remote card into one printable line of
// bounded length: it reaches the model and the --plugins listing.
func clean(s string, limit int) string {
	s = strings.Join(strings.Fields(termsafe.Text(s)), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}
