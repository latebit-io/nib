package nib

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/a2a"
)

func TestAssemble_A2A(t *testing.T) {
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	var err error
	h, err = a2a.NewServer(a2a.ServerConfig{Name: "peer", URL: srv.URL + "/", Token: "tok",
		Run: func(context.Context, []llm.Message) (string, error) { return "hi", nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := a2a.Config{
		Agents:  []a2a.Agent{{Name: "peer", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer tok"}, Source: "test"}, {Name: "gone", URL: "http://127.0.0.1:1/", Source: "test"}},
		Refused: []a2a.Refusal{{Agent: a2a.Agent{Name: "far", Source: "test"}, Err: errors.New("too far")}},
	}
	a, err := Assemble(context.Background(), Options{Dir: t.TempDir(), A2A: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	var tools []string
	for _, tool := range a.Toolset.Tools {
		tools = append(tools, tool.Definition().Function.Name)
	}
	if !slices.Contains(tools, "a2a__peer") || slices.Contains(tools, "a2a__gone") {
		t.Errorf("tools = %v", tools)
	}
	var rows []string
	for _, p := range a.Plugins {
		if strings.HasPrefix(p.Name, a2a.ToolNamePrefix) {
			rows = append(rows, p.Name+" "+p.Description)
		}
	}
	if joined := strings.Join(rows, "\n"); len(rows) != 3 || !strings.Contains(joined, "a2a__gone failed") || !strings.Contains(joined, "a2a__far refused") {
		t.Errorf("manifest rows = %q", rows)
	}
	if warnings := fmt.Sprint(a.Warnings); !strings.Contains(warnings, "A2A agent gone") || !strings.Contains(warnings, "A2A agent far not used") {
		t.Errorf("warnings = %s", warnings)
	}
}
