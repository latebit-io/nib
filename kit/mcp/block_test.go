package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// projectWith writes body as a project's config file and returns the
// project directory.
func projectWith(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProjectConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// names returns the server names, in order.
func names(servers []Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}

// reasons maps each refused server's name to its reason.
func reasons(cfg Config) map[string]string {
	out := map[string]string{}
	for _, r := range cfg.Refused {
		out[r.Server.Name] = r.Err.Error()
	}
	return out
}

const twoServers = `{"mcpServers":{"zeta":{"command":"zeta-server"},"alpha":{"command":"npx","args":["-y","alpha"],"env":{"TOKEN":"x"}}}}`

// TestDiscover_ProjectNeedsTrust: a repository's servers do not start
// until the caller says the project is trusted.
func TestDiscover_ProjectNeedsTrust(t *testing.T) {
	dir := projectWith(t, twoServers)

	cfg, err := Discover(DiscoverOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cfg.Servers) != 0 || len(cfg.Refused) != 2 {
		t.Fatalf("untrusted: %d servers, %d refused; want 0 and 2", len(cfg.Servers), len(cfg.Refused))
	}
	if err := cfg.Refused[0].Err; !errors.Is(err, ErrUntrusted) {
		t.Errorf("refusal = %v, want ErrUntrusted", err)
	}

	cfg, err = Discover(DiscoverOptions{ProjectDir: dir, TrustProject: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := strings.Join(names(cfg.Servers), ","); got != "alpha,zeta" {
		t.Errorf("servers = %s, want alpha,zeta in name order", got)
	}
	if s := cfg.Servers[0]; s.Origin != OriginProject || s.Env["TOKEN"] != "x" || len(s.Args) != 2 {
		t.Errorf("alpha = %+v, want the project's definition intact", s)
	}
}

// TestDiscover_RefusesLoaderEnv: a definition may not set a variable
// that makes its process load other code, whatever its origin.
func TestDiscover_RefusesLoaderEnv(t *testing.T) {
	dir := projectWith(t, `{"mcpServers":{
		"node":{"command":"npx server","env":{"NODE_OPTIONS":"--require ./x.js"}},
		"preload":{"command":"server","env":{"ld_preload":"./x.so"}},
		"path":{"command":"server","env":{"PATH":"./bin"}},
		"fine":{"command":"server","env":{"API_KEY":"k"}}}}`)

	cfg, err := Discover(DiscoverOptions{ProjectDir: dir, TrustProject: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := strings.Join(names(cfg.Servers), ","); got != "fine" {
		t.Errorf("servers = %s, want only fine", got)
	}
	why := reasons(cfg)
	for name, key := range map[string]string{"node": "NODE_OPTIONS", "preload": "ld_preload", "path": "PATH"} {
		if !strings.Contains(why[name], key) {
			t.Errorf("%s: reason %q does not name %s", name, why[name], key)
		}
	}
}

// TestDiscover_SourcesAndCollisions: the person's own list wins a name,
// plugin servers are prefixed, and a bad entry does not stop the rest.
func TestDiscover_SourcesAndCollisions(t *testing.T) {
	dir := projectWith(t, `{"mcpServers":{"docs":{"command":"project-docs"}}}`)
	pluginFile := filepath.Join(t.TempDir(), "plugin.json")
	if err := os.WriteFile(pluginFile, []byte(`{"mcpServers":{"docs":{"command":"plugin-docs"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	user, err := ParseSpec("docs=my-docs --flag; ;oops", "NIB_MCP")
	if err == nil || !strings.Contains(err.Error(), "oops") || len(user) != 1 {
		t.Errorf("ParseSpec = %v, %v; want one server and the bad entry reported", user, err)
	}
	cfg, err := Discover(DiscoverOptions{
		ProjectDir: dir, TrustProject: true, User: user,
		Plugins: []ConfigSource{{Prefix: "acme", Path: pluginFile}, {Prefix: "bad", Path: broken}},
	})
	if err == nil || !strings.Contains(err.Error(), "broken.json") {
		t.Errorf("err = %v, want the bad file reported", err)
	}
	if got := strings.Join(names(cfg.Servers), ","); got != "acme__docs,docs" {
		t.Fatalf("servers = %s, want acme__docs,docs", got)
	}
	if s := cfg.Servers[1]; s.Origin != OriginUser || s.Command != "my-docs" || !slices.Equal(s.Args, []string{"--flag"}) {
		t.Errorf("docs = %+v, want the person's own definition", s)
	}
	if r := reasons(cfg)["docs"]; !strings.Contains(r, "already used by a user server") {
		t.Errorf("project docs reason = %q, want the name collision", r)
	}
}

func TestDiscover_NothingConfigured(t *testing.T) {
	cfg, err := Discover(DiscoverOptions{ProjectDir: t.TempDir()})
	if err != nil || len(cfg.Servers) != 0 || len(cfg.Refused) != 0 {
		t.Errorf("Discover = %+v, %v; want empty and no error", cfg, err)
	}
}

// TestTrustStore_TiedToContent: approval covers the file as it was; a
// changed server list has to be approved again.
func TestTrustStore_TiedToContent(t *testing.T) {
	dir := projectWith(t, twoServers)
	store := OpenTrustStore(filepath.Join(t.TempDir(), "nested", "trust.json"))

	if ok, err := store.Trusted(dir); ok || err != nil {
		t.Fatalf("before Trust: %v, %v; want false, nil", ok, err)
	}
	cfg, err := store.Approve(dir)
	if err != nil || len(cfg.Servers) != 2 {
		t.Fatalf("Approve = %+v, %v; want the two servers allowed", cfg, err)
	}
	if ok, err := store.Trusted(dir); !ok || err != nil {
		t.Fatalf("after Trust: %v, %v; want true, nil", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProjectConfigName), []byte(`{"mcpServers":{"evil":{"command":"curl"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Trusted(dir); ok || err != nil {
		t.Errorf("after the file changed: %v, %v; want false, nil", ok, err)
	}
	// A second project does not disturb the first one's entry.
	other := projectWith(t, twoServers)
	if _, err := store.Approve(other); err != nil {
		t.Fatalf("Approve(other): %v", err)
	}
	if ok, _ := store.Trusted(other); !ok {
		t.Error("the second project must be trusted")
	}
}

func TestTrustStore_NothingToTrust(t *testing.T) {
	store := OpenTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	dir := t.TempDir()
	if _, err := store.Approve(dir); err == nil {
		t.Error("Approve on a project without a config file must fail")
	}
	if ok, err := store.Trusted(dir); ok || err != nil {
		t.Errorf("Trusted = %v, %v; want false, nil", ok, err)
	}
}

// TestConnect_ToolsAndFailures: a working server's tools are offered
// under prefixed names, and a server that cannot start is recorded
// without costing the session the working one.
func TestConnect_ToolsAndFailures(t *testing.T) {
	script := writeMockServer(t)
	cfg := Config{
		Servers: []Server{
			{Name: "broken", Command: filepath.Join(t.TempDir(), "no-such-binary"), Origin: OriginUser},
			{Name: "mock", Command: "sh", Args: []string{script}, Origin: OriginUser},
		},
		Refused: []Refusal{{Server: Server{Name: "repo", Origin: OriginProject, Source: ".mcp.json"}, Err: ErrUntrusted}},
	}
	s := Connect(context.Background(), cfg.Servers, ConnectOptions{})
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	tools := s.Tools()
	if len(tools) != 1 || len(s.Servers()) != 1 || len(s.Failed) != 1 {
		t.Fatalf("tools %d, connected %d, failed %d; want 1 each", len(tools), len(s.Servers()), len(s.Failed))
	}
	def := tools[0].Definition().Function
	if def.Name != "mcp__mock__mark_fetch" {
		t.Errorf("tool name = %q, want mcp__mock__mark_fetch", def.Name)
	}
	if p := def.Parameters; p.Properties["url"].Type != "string" || len(p.Required) != 1 {
		t.Errorf("parameters = %+v, want the server's schema", p)
	}
	res := tools[0].Execute(context.Background(), llm.ToolCall{Function: llm.FunctionCall{Name: def.Name, Arguments: `{"url":"/x"}`}})
	if res.IsError || res.Content != "hello from mock" {
		t.Errorf("Execute = %+v, want the server's reply", res)
	}

	out := kit.RenderPlugins(Plugins(cfg, s))
	for _, want := range []string{"mcp__mock__mark_fetch", "[mcp:mock]", "mcp__broken", "failed:", "mcp__repo", "refused: this project's servers are not trusted"} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest lacks %q:\n%s", want, out)
		}
	}
}

func TestSession_NilClose(t *testing.T) {
	var s *Session
	if err := s.Close(); err != nil {
		t.Errorf("Close on nil = %v", err)
	}
}

// fakeCaller records a call and returns a canned result.
type fakeCaller struct {
	result ToolResult
	name   string
	args   map[string]any
}

func (f *fakeCaller) CallToolResult(_ context.Context, name string, args map[string]any) (ToolResult, error) {
	f.name, f.args = name, args
	return f.result, nil
}

func call(args string) llm.ToolCall {
	return llm.ToolCall{Function: llm.FunctionCall{Arguments: args}}
}

func TestTool_ExecuteForwardsAndCaps(t *testing.T) {
	fake := &fakeCaller{result: ToolResult{Text: strings.Repeat("é", maxResultBytes), IsError: true}}
	exact, err := newTool(fake, "s", ToolInfo{Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// 2^53+1 is not a float64; the server must get the digits the model wrote.
	exact.Execute(context.Background(), call(`{"id":9007199254740993}`))
	if got := fmt.Sprint(fake.args["id"]); got != "9007199254740993" {
		t.Errorf("id reached the server as %s, want 9007199254740993", got)
	}
	if res := exact.Execute(context.Background(), call(`{"id":1} {"id":2}`)); !res.IsError {
		t.Error("content after the arguments object must be an error result")
	}
	tl, err := newTool(fake, "my server", ToolInfo{Name: "do.it"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tl.Definition().Function.Name; got != "mcp__my_server__do_it" {
		t.Errorf("name = %q, want the sanitized form", got)
	}
	res := tl.Execute(context.Background(), call(""))
	if fake.name != "do.it" || fake.args == nil {
		t.Errorf("server saw %q %v, want its own tool name and an empty object", fake.name, fake.args)
	}
	if !res.IsError {
		t.Error("a failure the server reports must be an error result")
	}
	if !strings.HasSuffix(res.Content, "[... output truncated]") || len(res.Content) > maxResultBytes+32 {
		t.Errorf("reply not capped: %d bytes", len(res.Content))
	}
	if res := tl.Execute(context.Background(), call("{not json")); !res.IsError {
		t.Error("invalid arguments must be an error result")
	}
	if res := tl.Execute(context.Background(), call(strings.Repeat("x", maxArgBytes+1))); !res.IsError {
		t.Error("oversize arguments must be an error result")
	}
}

func TestTool_NameTooLong(t *testing.T) {
	if _, err := newTool(&fakeCaller{}, "server", ToolInfo{Name: strings.Repeat("n", maxToolName)}); err == nil {
		t.Error("a name over the provider limit must be refused")
	}
}

// TestParameters: a nested schema passes through; one the typed form
// cannot hold becomes an open object, never a property with no type.
func TestParameters(t *testing.T) {
	nested := `{"type":"object","required":["items"],"properties":{"items":{"type":"array","description":"rows","items":{"type":"object","properties":{"id":{"type":"integer"},"tag":{"type":["string","null"],"enum":["a","b"]}}}}}}`
	p := parameters("t", json.RawMessage(nested))
	item := p.Properties["items"].Items
	if item == nil || item.Properties["id"].Type != "integer" || !item.Properties["tag"].Nullable || len(p.Required) != 1 {
		t.Errorf("nested schema lost detail: %+v", p)
	}
	for name, schema := range map[string]string{
		"anyOf":          `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`,
		"array no items": `{"type":"object","properties":{"v":{"type":"array"}}}`,
		"nested ref":     `{"type":"object","properties":{"v":{"type":"object","properties":{"w":{"$ref":"#/$defs/x"}}}}}`,
		"not an object":  `[1,2]`,
	} {
		p := parameters("t", json.RawMessage(schema))
		if p.Type != "object" || len(p.Properties) != 0 {
			t.Errorf("%s: parameters = %+v, want an open object", name, p)
		}
	}
	if p := parameters("t", nil); p.Type != "object" {
		t.Errorf("no schema: Type = %q, want object", p.Type)
	}
}

// TestDiscover_TrustCoversTheParsedBytes: with a trust store, the
// project's servers start only while the file is the one approved.
func TestDiscover_TrustCoversTheParsedBytes(t *testing.T) {
	dir := projectWith(t, twoServers)
	store := OpenTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	allowed := func() int {
		t.Helper()
		cfg, err := Discover(DiscoverOptions{ProjectDir: dir, Trust: store})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		return len(cfg.Servers)
	}
	if n := allowed(); n != 0 {
		t.Fatalf("before approval: %d servers allowed, want 0", n)
	}
	if _, err := store.Approve(dir); err != nil {
		t.Fatal(err)
	}
	if n := allowed(); n != 2 {
		t.Fatalf("after approval: %d servers allowed, want 2", n)
	}
	if err := os.WriteFile(filepath.Join(dir, ProjectConfigName), []byte(`{"mcpServers":{"evil":{"command":"curl"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := allowed(); n != 0 {
		t.Errorf("after the file changed: %d servers allowed, want 0", n)
	}
}

// TestConnect_CommandWithSpaces: a configured command is one program,
// even when its path holds spaces.
func TestConnect_CommandWithSpaces(t *testing.T) {
	script, err := os.ReadFile(writeMockServer(t))
	if err != nil {
		t.Fatal(err)
	}
	spaced := filepath.Join(t.TempDir(), "MCP Tools", "server")
	if err := os.MkdirAll(filepath.Dir(spaced), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spaced, script, 0o755); err != nil {
		t.Fatal(err)
	}
	s := Connect(context.Background(), []Server{{Name: "spaced", Command: spaced}}, ConnectOptions{})
	t.Cleanup(func() { _ = s.Close() }) // test cleanup; the assertions below are the test
	if len(s.Tools()) != 1 || len(s.Failed) != 0 {
		t.Errorf("tools %d, failed %v; want the server started", len(s.Tools()), s.Failed)
	}
}

// TestConnect_CollidingToolNames: two servers whose names sanitize to
// the same text cannot both offer a tool; the second is reported.
func TestConnect_CollidingToolNames(t *testing.T) {
	script := writeMockServer(t)
	s := Connect(context.Background(), []Server{
		{Name: "a.b", Command: "sh", Args: []string{script}},
		{Name: "a_b", Command: "sh", Args: []string{script}},
	}, ConnectOptions{})
	t.Cleanup(func() { _ = s.Close() }) // test cleanup; the assertions below are the test
	if len(s.Tools()) != 1 || len(s.Failed) != 1 || !strings.Contains(s.Failed[0].Err.Error(), "already taken") {
		t.Errorf("tools %d, failed %v; want one tool and the collision reported", len(s.Tools()), s.Failed)
	}
	if servers := s.Servers(); len(servers) != 2 || len(servers[1].Tools) != 0 {
		t.Errorf("the second server must stay connected without the colliding tool: %+v", servers)
	}
}
