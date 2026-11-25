package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llmtest"
)

type findArgs struct {
	Query string `json:"query" desc:"substring to match"`
	Limit *int   `json:"limit,omitempty" min:"1" max:"25"`
}

// TestNewTool covers what reaches run and what the model is told instead.
func TestNewTool(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		runErr    error
		wantArgs  *findArgs // nil when run must not be called
		want      string
		wantError bool
	}{
		{name: "decoded arguments reach run", args: `{"query":"q","limit":3}`,
			wantArgs: &findArgs{Query: "q", Limit: new(3)}, want: "found"},
		{name: "optional argument left out", args: `{"query":"q"}`,
			wantArgs: &findArgs{Query: "q"}, want: "found"},
		{name: "run's error becomes an error result", args: `{"query":"q"}`, runErr: errors.New("index offline"),
			wantArgs: &findArgs{Query: "q"}, want: "Error: index offline", wantError: true},
		{name: "missing required argument", args: `{}`,
			want: "Error: invalid arguments: query: required", wantError: true},
		{name: "no arguments at all", args: "",
			want: "Error: invalid arguments: query: required", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got *findArgs
			tool := NewTool("find", "Find documents.", func(_ context.Context, args findArgs) (string, error) {
				got = &args
				return "found", tc.runErr
			})

			result := tool.Execute(context.Background(), llmtest.Call("c1", "find", tc.args))

			if !strings.HasPrefix(result.Content, tc.want) || result.IsError != tc.wantError {
				t.Errorf("result = %+v; want %q, error %v", result, tc.want, tc.wantError)
			}
			if !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("run got %+v; want %+v", got, tc.wantArgs)
			}
		})
	}
}

func TestNewTool_Definition(t *testing.T) {
	tool := NewTool("find", "Find documents.", func(context.Context, findArgs) (string, error) { return "", nil })

	got, err := json.Marshal(tool.Definition())
	want := `{"type":"function","function":{"name":"find","description":"Find documents.","parameters":{"type":"object",` +
		`"properties":{"limit":{"type":"integer","description":"","minimum":1,"maximum":25},` +
		`"query":{"type":"string","description":"substring to match"}},"required":["query"]}}}`
	if err != nil || string(got) != want {
		t.Errorf("Definition = %s, %v\nwant %s", got, err, want)
	}
}

func TestNewTool_PanicsOnUndescribableArguments(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), `tool "bad"`) {
			t.Errorf("recovered %v; want a panic naming the tool", r)
		}
	}()
	NewTool("bad", "", func(context.Context, struct{ V any }) (string, error) { return "", nil })
}
