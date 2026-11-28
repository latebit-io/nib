package a2a

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/internal/httporigin"
)

// The server keeps this many conversations, each up to maxTurns messages.
const (
	maxContexts = 64
	maxTurns    = 200
)

// maxRequestBytes caps a request body: a message is text a person or
// an agent wrote.
const maxRequestBytes = 1 << 20

// cardCacheControl lets clients keep the card a few minutes.
const cardCacheControl = "max-age=300"

// bearerScheme names the card's one security scheme.
const bearerScheme a2a.SecuritySchemeName = "bearer"

// ServerConfig describes the agent [NewServer] serves.
type ServerConfig struct {
	// Name, Description and Version go on the card. Name is required.
	Name, Description, Version string
	// URL is where clients reach the handler, as the card states it.
	URL string
	// Skills are listed on the card.
	Skills []Skill
	// Token is the bearer token every request but the card's must carry.
	// Required: the card is public, the agent is not.
	Token string
	// Run answers one message. history is the conversation so far, user
	// and assistant turns, ending with the new message. Calls never overlap.
	Run func(ctx context.Context, history []llm.Message) (string, error)
}

// Skill is one capability listed on the card.
type Skill struct {
	ID, Name, Description string
	Tags                  []string
}

// ServeURL is the address clients reach a server listening on addr at.
// Only an address on this machine will do: the server speaks plain http.
func ServeURL(addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("a2a: listen address %s: %w", addr, err)
	}
	if !httporigin.Loopback(host) {
		return "", fmt.Errorf("a2a: %s is reachable from other machines; serve on this machine only (127.0.0.1)", addr)
	}
	return "http://" + addr + "/", nil
}

// NewServer returns the handler for cfg: the card at the well-known
// path, the agent over JSON-RPC at every other path.
func NewServer(cfg ServerConfig) (http.Handler, error) {
	switch {
	case cfg.Token == "":
		return nil, errors.New("a2a: a server needs a token: the card is public, the agent is not")
	case cfg.Run == nil:
		return nil, errors.New("a2a: a server needs Run")
	case cfg.Name == "":
		return nil, errors.New("a2a: a server needs a name")
	}
	// The card names this address, and clients send the token to it.
	if err := httporigin.CheckEndpoint(cfg.URL); err != nil {
		return nil, fmt.Errorf("a2a: server URL: %w", err)
	}
	card := &a2a.AgentCard{
		Name:                cfg.Name,
		Description:         cfg.Description,
		Version:             cmp.Or(cfg.Version, "0"),
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(cfg.URL, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{Streaming: true},
		Skills:              make([]a2a.AgentSkill, 0, len(cfg.Skills)),
		SecuritySchemes:     a2a.NamedSecuritySchemes{bearerScheme: a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
		SecurityRequirements: a2a.SecurityRequirementsOptions{
			{bearerScheme: {}},
		},
	}
	for _, s := range cfg.Skills {
		tags := s.Tags
		if tags == nil {
			tags = []string{} // the card requires the field
		}
		card.Skills = append(card.Skills, a2a.AgentSkill{ID: s.ID, Name: s.Name, Description: s.Description, Tags: tags})
	}
	exec := &executor{run: cfg.Run, contexts: map[string][]llm.Message{}}
	enforcer, err := a2asrv.NewAgentCardEnforcer(card)
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	handler := a2asrv.NewHandler(exec, a2asrv.WithCallInterceptors(enforcer))
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card, a2asrv.WithAgentCardCacheControl(cardCacheControl)))
	mux.Handle("/", bearer(cfg.Token, a2asrv.NewJSONRPCHandler(handler)))
	return mux, nil
}

// bearer lets a request through only with the token.
func bearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="a2a"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

// executor runs the agent for each message, one at a time, and keeps
// each context's conversation.
type executor struct {
	run func(ctx context.Context, history []llm.Message) (string, error)

	mu       sync.Mutex // held for a whole run: one workspace, one run
	contexts map[string][]llm.Message
	order    []string // context IDs, oldest first
}

// Execute implements [a2asrv.AgentExecutor].
func (e *executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		ask, err := userText(ec.Message)
		if err != nil {
			yield(status(ec, a2a.TaskStateRejected, err.Error()), nil)
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		history := append(slices.Clone(e.contexts[ec.ContextID]), llm.Message{Role: "user", Content: ask})
		answer, err := e.run(ctx, history)
		if err != nil {
			yield(status(ec, a2a.TaskStateFailed, err.Error()), nil)
			return
		}
		e.keep(ec.ContextID, append(history, llm.Message{Role: "assistant", Content: answer}))
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(answer)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

// Cancel implements [a2asrv.AgentExecutor]. The server stack cancels
// the run's context once the canceled state is stored.
func (e *executor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// keep stores the conversation for id, dropping its oldest turns past
// maxTurns and the oldest conversation past maxContexts.
func (e *executor) keep(id string, history []llm.Message) {
	if len(history) > maxTurns {
		history = history[len(history)-maxTurns:]
	}
	if _, known := e.contexts[id]; !known {
		e.order = append(e.order, id)
		if len(e.order) > maxContexts {
			delete(e.contexts, e.order[0])
			e.order = e.order[1:]
		}
	}
	e.contexts[id] = history
}

// userText is the text of a message, which must be text only.
func userText(m *a2a.Message) (string, error) {
	if m == nil {
		return "", errors.New("no message")
	}
	var out []string
	for _, p := range m.Parts {
		if p == nil {
			continue
		}
		t := p.Text()
		if t == "" {
			return "", errors.New("this agent reads text parts only")
		}
		out = append(out, t)
	}
	text := strings.TrimSpace(strings.Join(out, "\n"))
	if text == "" {
		return "", errors.New("the message has no text")
	}
	return text, nil
}

// status is a status update carrying an agent message.
func status(ec *a2asrv.ExecutorContext, state a2a.TaskState, text string) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(ec, state, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(text)))
}
