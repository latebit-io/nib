package mcpadapter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/memory"
)

// stubCaller records calls and returns canned results keyed by tool name.
type stubCaller struct {
	responses map[string]mcp.ToolResult
	errs      map[string]error
	calls     []call
}

type call struct {
	name string
	args map[string]any
}

func (s *stubCaller) CallToolResult(_ context.Context, name string, args map[string]any) (mcp.ToolResult, error) {
	s.calls = append(s.calls, call{name: name, args: args})
	if err, ok := s.errs[name]; ok {
		return mcp.ToolResult{}, err
	}
	if r, ok := s.responses[name]; ok {
		return r, nil
	}
	return mcp.ToolResult{}, errors.New("stubCaller: no response configured for " + name)
}

func newAdapter(c caller) *Adapter {
	return &Adapter{client: c}
}

func TestFetchSuccess(t *testing.T) {
	stub := &stubCaller{responses: map[string]mcp.ToolResult{
		"mark_fetch": {Text: "status: ok\nversion: 5\nmodified: 2026-04-16T10:00:00Z\netag: abcd\n\n# Title\nbody line", IsError: false},
	}}
	a := newAdapter(stub)
	doc, err := a.Fetch(context.Background(), "/index.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Path != "/index.md" {
		t.Errorf("Path: got %q, want /index.md", doc.Path)
	}
	if doc.Version != 5 {
		t.Errorf("Version: got %d, want 5", doc.Version)
	}
	if doc.Modified != "2026-04-16T10:00:00Z" {
		t.Errorf("Modified: got %q", doc.Modified)
	}
	if doc.Body != "# Title\nbody line" {
		t.Errorf("Body: got %q", doc.Body)
	}
	if len(stub.calls) != 1 || stub.calls[0].name != "mark_fetch" {
		t.Errorf("expected one mark_fetch call, got %+v", stub.calls)
	}
	if stub.calls[0].args["url"] != "/index.md" {
		t.Errorf("url arg: got %v, want /index.md", stub.calls[0].args["url"])
	}
}

func TestFetchStatusMapping(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		isError bool
		wantErr error
	}{
		{"not found", "status: not-found\n", false, memory.ErrNotFound},
		{"conflict", "status: conflict\nserver-version: 7\n", false, memory.ErrConflict},
		{"unauthorized", "status: unauthorized\n", false, memory.ErrAuth},
		{"archived", "status: archived\n", false, memory.ErrNotFound},
		{"handler error", "fetch failed: dial tcp: connection refused", true, memory.ErrServer},
		{"unknown status", "status: teapot\n", false, memory.ErrServer},
		{"missing status", "\n", false, memory.ErrServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubCaller{responses: map[string]mcp.ToolResult{
				"mark_fetch": {Text: tt.text, IsError: tt.isError},
			}}
			a := newAdapter(stub)
			_, err := a.Fetch(context.Background(), "/missing.md")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want error containing %v", err, tt.wantErr)
			}
		})
	}
}

func TestPublishArgs(t *testing.T) {
	stub := &stubCaller{responses: map[string]mcp.ToolResult{
		"mark_publish": {Text: "status: created\nversion: 1\nmodified: 2026-04-16T10:00:00Z\n\n"},
	}}
	a := newAdapter(stub)
	doc, err := a.Publish(context.Background(), "/new.md", "# New doc", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("Version: got %d, want 1", doc.Version)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(stub.calls))
	}
	args := stub.calls[0].args
	if args["url"] != "/new.md" {
		t.Errorf("url: got %v", args["url"])
	}
	if args["body"] != "# New doc" {
		t.Errorf("body: got %v", args["body"])
	}
	if args["expected_version"] != 0 {
		t.Errorf("expected_version: got %v, want 0", args["expected_version"])
	}
}

func TestAppendArgs(t *testing.T) {
	stub := &stubCaller{responses: map[string]mcp.ToolResult{
		"mark_append": {Text: "status: ok\nversion: 3\nmodified: 2026-04-16T10:00:00Z\n\n"},
	}}
	a := newAdapter(stub)
	_, err := a.Append(context.Background(), "/journal.md", "\nnew line\n", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := stub.calls[0].args
	if args["expected_version"] != 2 {
		t.Errorf("expected_version: got %v, want 2", args["expected_version"])
	}
}

func TestListExtractsLinks(t *testing.T) {
	// mark_list returns a markdown index with relative link targets.
	body := `# /docs/

- [architecture.md](architecture.md)
- [subdir/](subdir/)
- absolute: [index.md](/index.md)
- non-link line
`
	stub := &stubCaller{responses: map[string]mcp.ToolResult{
		"mark_list": {Text: "status: ok\nmodified: 2026-04-16T10:00:00Z\n\n" + body},
	}}
	a := newAdapter(stub)
	got, err := a.List(context.Background(), "/docs/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"/docs/architecture.md", "/docs/subdir/", "/index.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestListHandlerError(t *testing.T) {
	stub := &stubCaller{responses: map[string]mcp.ToolResult{
		"mark_list": {Text: "list failed: bad host", IsError: true},
	}}
	a := newAdapter(stub)
	_, err := a.List(context.Background(), "/bad/")
	if !errors.Is(err, memory.ErrServer) {
		t.Fatalf("expected ErrServer, got %v", err)
	}
}

// TestListStatusHeader proves List honors the status header the same
// way document calls do, instead of parsing a not-found body as links.
func TestListStatusHeader(t *testing.T) {
	cases := []struct {
		status string
		want   error
	}{
		{"not-found", memory.ErrNotFound},
		{"unauthorized", memory.ErrAuth},
		{"", memory.ErrServer},
		{"weird", memory.ErrServer},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			text := "status: " + tc.status + "\n\n- [x.md](x.md)\n"
			if tc.status == "" {
				text = "- [x.md](x.md)\n"
			}
			stub := &stubCaller{responses: map[string]mcp.ToolResult{"mark_list": {Text: text}}}
			_, err := newAdapter(stub).List(context.Background(), "/docs/")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %q: err = %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestTransportError(t *testing.T) {
	wantErr := errors.New("mcp: connection closed")
	stub := &stubCaller{errs: map[string]error{"mark_fetch": wantErr}}
	a := newAdapter(stub)
	_, err := a.Fetch(context.Background(), "/x")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped transport error, got %v", err)
	}
}

func TestParseResultNoBody(t *testing.T) {
	// Response without a blank separator — headers only.
	h, body, err := parseResult("status: not-found\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h["status"] != "not-found" {
		t.Errorf("status: got %q", h["status"])
	}
	if body != "" {
		t.Errorf("body: got %q, want empty", body)
	}
}

func TestParseResultMalformedHeader(t *testing.T) {
	// A line with no colon before the blank separator should error.
	_, _, err := parseResult("bogus line\n\nbody")
	if !errors.Is(err, memory.ErrServer) {
		t.Fatalf("expected ErrServer, got %v", err)
	}
}
