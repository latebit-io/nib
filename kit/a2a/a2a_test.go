package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/latebit-io/nib/ai/llm"
)

const testToken = "s3cret"

func TestLoadConfig(t *testing.T) {
	t.Setenv("NIB_TEST_A2A_TOKEN", "abc")
	path := filepath.Join(t.TempDir(), "a2a.json")
	file := `{"agents": {
		"helper": {"url": "https://helper.example/", "headers": {"Authorization": "Bearer ${NIB_TEST_A2A_TOKEN}"}},
		"both": {"url": "https://from-file.example/"},
		"plain": {"url": "http://far.example/"},
		"bad name": {"url": "https://x.example/"},
		"nourl": {},
		"unset": {"url": "https://x.example/", "headers": {"X": "${NIB_TEST_A2A_UNSET}"}}
	}}`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	env := `{"agents": {"both": {"url": "https://from-env.example/"}, "local": {"url": "http://127.0.0.1:9/"}}}`
	cfg, err := LoadConfig(path, env)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Agent{}
	for _, a := range cfg.Agents {
		got[a.Name] = a
	}
	if len(got) != 3 || got["helper"].Headers["Authorization"] != "Bearer abc" || got["both"].URL != "https://from-env.example/" || got["both"].Source != EnvKey {
		t.Errorf("agents = %+v", cfg.Agents)
	}
	var refused []string
	for _, r := range cfg.Refused {
		refused = append(refused, r.Agent.Name)
	}
	if want := []string{"bad name", "nourl", "plain", "unset"}; !slices.Equal(refused, want) {
		t.Errorf("refused %v; want %v", refused, want)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), ""); err != nil {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := LoadConfig("", "{"); err == nil {
		t.Error("a bad variable was accepted")
	}
	// A list that half decodes is dropped whole: the file's agent stays.
	partial := `{"agents": {"helper": {"url": "https://env.example/", "headers": 5}}}`
	cfg, err = LoadConfig(path, partial)
	if err == nil {
		t.Error("a bad variable was accepted")
	}
	for _, a := range cfg.Agents {
		if a.Name == "helper" && a.URL != "https://helper.example/" {
			t.Errorf("a half-decoded list replaced the file's agent: %+v", a)
		}
	}
}

// serve starts handler-to-be at a loopback address and returns it.
func serve(t *testing.T, build func(url string) http.Handler) string {
	t.Helper()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	h = build(srv.URL + "/")
	return srv.URL + "/"
}

// nibServer serves an agent that answers with how many turns it was given.
func nibServer(t *testing.T, seen *[][]llm.Message) string {
	t.Helper()
	var mu sync.Mutex
	return serve(t, func(url string) http.Handler {
		h, err := NewServer(ServerConfig{Name: "nib", Description: "test agent", URL: url, Token: testToken,
			Skills: []Skill{{ID: "general", Name: "General", Description: "anything"}},
			Run: func(_ context.Context, history []llm.Message) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				*seen = append(*seen, history)
				if history[len(history)-1].Content == "fail" {
					return "", errors.New("the model is gone")
				}
				return fmt.Sprintf("turns: %d", len(history)), nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	})
}

// call runs tool with args.
func call(t *testing.T, r *remote, args string) (string, bool) {
	t.Helper()
	res := r.Execute(context.Background(), llm.ToolCall{ID: "1", Type: "function", Function: llm.FunctionCall{Name: r.def.Function.Name, Arguments: args}})
	return res.Content, res.IsError
}

// idsPattern finds the conversation and task an answer names.
var idsPattern = regexp.MustCompile(`(?i)conversation "([^"]*)"(?: and task "([^"]*)")?`)

// ids returns the conversation and task out names.
func ids(t *testing.T, out string) (conversation, task string) {
	t.Helper()
	m := idsPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("%q names no conversation", out)
	}
	return m[1], m[2]
}

// connectOne connects to the agent at url and returns its tool.
func connectOne(t *testing.T, url string, headers map[string]string) *remote {
	t.Helper()
	s := Connect(context.Background(), Config{Agents: []Agent{{Name: "peer", URL: url, Headers: headers, Source: "test"}}})
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if len(s.Failed) != 0 {
		t.Fatalf("connect: %v", s.Failed[0].Err)
	}
	return s.agents[0]
}

func TestRoundTrip(t *testing.T) {
	var seen [][]llm.Message
	url := nibServer(t, &seen)
	r := connectOne(t, url, map[string]string{"Authorization": "Bearer " + testToken})
	if d := r.Definition().Function.Description; !strings.Contains(d, "test agent") || !strings.Contains(d, "General") {
		t.Errorf("description %q lacks the card", d)
	}
	first, isErr := call(t, r, `{"message":"hello"}`)
	if isErr || !strings.HasPrefix(first, "turns: 1") {
		t.Fatalf("first = %q, %v", first, isErr)
	}
	conv, _ := ids(t, first)
	if got, isErr := call(t, r, `{"message":"more","conversation":"`+conv+`"}`); isErr || !strings.HasPrefix(got, "turns: 3") {
		t.Errorf("follow-up = %q, %v", got, isErr)
	}
	// Without the conversation, a call starts its own: callers stay apart.
	if got, _ := call(t, r, `{"message":"again"}`); !strings.HasPrefix(got, "turns: 1") {
		t.Errorf("a new conversation = %q", got)
	}
	if got, isErr := call(t, r, `{"message":"fail"}`); !isErr || !strings.Contains(got, "the model is gone") {
		t.Errorf("a failed run = %q, %v", got, isErr)
	}
	// The task store answers only a known caller: the token must make one.
	if list, err := r.client.ListTasks(context.Background(), &a2a.ListTasksRequest{}); err != nil || len(list.Tasks) == 0 {
		t.Errorf("list tasks = %+v, %v", list, err)
	}
	if len(seen) < 2 || seen[1][0].Content != "hello" || seen[1][1].Role != "assistant" {
		t.Errorf("history = %+v", seen)
	}
}

func TestServer_Token(t *testing.T) {
	run := func(context.Context, []llm.Message) (string, error) { return "", nil }
	if _, err := NewServer(ServerConfig{Name: "nib", URL: "http://127.0.0.1:1/", Run: run}); err == nil {
		t.Error("a server without a token was built")
	}
	if _, err := NewServer(ServerConfig{Name: "nib", URL: "http://192.168.1.2:1/", Token: "t", Run: run}); err == nil {
		t.Error("a plain http server on the network was built")
	}
	var seen [][]llm.Message
	url := nibServer(t, &seen)
	resp, err := http.Get(url + ".well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		Name            string         `json:"name"`
		SecuritySchemes map[string]any `json:"securitySchemes"`
	}
	err = json.NewDecoder(resp.Body).Decode(&card)
	if cerr := resp.Body.Close(); cerr != nil {
		t.Error(cerr)
	}
	if err != nil || resp.StatusCode != http.StatusOK || card.Name != "nib" || card.SecuritySchemes["bearer"] == nil {
		t.Errorf("card: %d %+v %v", resp.StatusCode, card, err)
	}
	for _, headers := range []map[string]string{nil, {"Authorization": "Bearer wrong"}} {
		r := connectOne(t, url, headers)
		if got, isErr := call(t, r, `{"message":"hello"}`); !isErr {
			t.Errorf("without the token: %q", got)
		}
	}
	if len(seen) != 0 {
		t.Errorf("ran without the token: %v", seen)
	}
}

func TestServeURL(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:1": true, "[::1]:80": true,
		":8765": false, "0.0.0.0:80": false, "127.0.0.1:0": false, "127.0.0.1:": false, "127.0.0.1:http": false, "192.168.1.2:80": false, "example.com:80": false, "nope": false,
	} {
		if _, err := ServeURL(addr); (err == nil) != ok {
			t.Errorf("%s: %v; want accepted %v", addr, err, ok)
		}
	}
}

func TestToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nib", "a2a-token")
	first, err := Token(path)
	if err != nil || first == "" {
		t.Fatalf("first = %q, %v", first, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info, err)
	}
	// A second writer, as from a server started at once, keeps the first token.
	if _, err := newToken(path); !errors.Is(err, fs.ErrExist) {
		t.Errorf("a second token over the first: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 {
		t.Errorf("files left beside the token: %v, %v", entries, err)
	}
	if again, err := Token(path); err != nil || again != first {
		t.Errorf("again = %q, %v; want the same token", again, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(path); err == nil {
		t.Error("a token others may read was used")
	}
	empty := filepath.Join(t.TempDir(), "a2a-token")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(empty); err == nil {
		t.Error("an empty token was used")
	}
}

// sdkServer serves exec as an agent whose card names iface(url).
func sdkServer(t *testing.T, exec a2asrv.AgentExecutor, iface func(url string) string) string {
	t.Helper()
	return serve(t, func(url string) http.Handler {
		card := &a2a.AgentCard{Name: "other", SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(iface(url), a2a.TransportProtocolJSONRPC)}}
		mux := http.NewServeMux()
		mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
		mux.Handle("/", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)))
		return mux
	})
}

// executorFunc adapts a function to [a2asrv.AgentExecutor].
type executorFunc func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error]

func (f executorFunc) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return f(ctx, ec)
}

func (f executorFunc) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return (*executor)(nil).Cancel(ctx, ec)
}

func TestTool_InputRequired(t *testing.T) {
	var mu sync.Mutex
	var tasks []a2a.TaskID
	exec := executorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			mu.Lock()
			tasks = append(tasks, ec.TaskID)
			mu.Unlock()
			if ec.StoredTask == nil {
				if yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
					yield(status(ec, a2a.TaskStateInputRequired, "which file?"), nil)
				}
				return
			}
			if yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("done with "+ec.Message.Parts[0].Text())), nil) {
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
			}
		}
	})
	r := connectOne(t, sdkServer(t, exec, func(url string) string { return url }), nil)
	first, isErr := call(t, r, `{"message":"fix it"}`)
	if isErr || !strings.Contains(first, "which file?") {
		t.Fatalf("first call = %q, %v", first, isErr)
	}
	conv, task := ids(t, first)
	if task == "" {
		t.Fatalf("%q names no waiting task", first)
	}
	if got, isErr := call(t, r, `{"message":"main.go","conversation":"`+conv+`","task":"`+task+`"}`); isErr || !strings.HasPrefix(got, "done with main.go") {
		t.Errorf("answer = %q, %v", got, isErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tasks) != 2 || tasks[0] != tasks[1] {
		t.Errorf("tasks %v; want the answer on the first task", tasks)
	}
}

func TestConnect_OtherOrigin(t *testing.T) {
	exec := executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) {}
	})
	url := sdkServer(t, exec, func(string) string { return "https://elsewhere.example/" })
	s := Connect(context.Background(), Config{Agents: []Agent{{Name: "peer", URL: url}}})
	if len(s.Failed) != 1 || !strings.Contains(s.Failed[0].Err.Error(), "no interface on") || len(s.Tools()) != 0 {
		t.Errorf("failed = %v, tools = %d", s.Failed, len(s.Tools()))
	}
	// The transport refuses another origin even if a request names one.
	c := &confined{origin: "http://127.0.0.1:1"}
	req, err := http.NewRequest(http.MethodGet, "https://elsewhere.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.RoundTrip(req); err == nil {
		t.Errorf("a request off the origin went out: %v", resp.Status)
	}
}

func TestConnect_ResponseTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(`{"name":"` + strings.Repeat("x", maxResponseBytes) + `"}`))
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	s := Connect(context.Background(), Config{Agents: []Agent{{Name: "big", URL: srv.URL}}})
	if len(s.Failed) != 1 || !strings.Contains(s.Failed[0].Err.Error(), "over") {
		t.Errorf("failed = %v", s.Failed)
	}
}

func TestTool_Timeout(t *testing.T) {
	release := make(chan struct{})
	exec := executorFunc(func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(func(a2a.Event, error) bool) { <-release }
	})
	r := connectOne(t, sdkServer(t, exec, func(url string) string { return url }), nil)
	t.Cleanup(func() { close(release) }) // before the server closes: it waits for the call
	r.timeout = 200 * time.Millisecond
	if got, isErr := call(t, r, `{"message":"hello"}`); !isErr || !strings.Contains(got, "no answer within") {
		t.Errorf("a silent agent = %q, %v", got, isErr)
	}
}
