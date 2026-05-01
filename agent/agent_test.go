package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// stubProvider is the minimum llm.Provider stand-in for construction tests.
type stubProvider struct{}

func (stubProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// namedTool is a Tool stub that reports a configurable Definition.
// Used to drive table-driven New validation cases without each row
// needing a bespoke type.
type namedTool struct {
	name string
}

func (t namedTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type:     "function",
		Function: llm.FunctionDef{Name: t.name},
	}
}

func (t namedTool) Execute(_ context.Context, _ llm.ToolCall) ToolResult {
	return ToolResult{}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	provider := stubProvider{}
	events := make(chan event.Event, 1)

	cases := []struct {
		name      string
		opts      Options
		wantError string // empty means construction must succeed
	}{
		{
			name:      "nil provider rejected",
			opts:      Options{Events: events},
			wantError: "Provider is required",
		},
		{
			name:      "nil events rejected",
			opts:      Options{Provider: provider},
			wantError: "Events is required",
		},
		{
			name: "nil tool entry rejected",
			opts: Options{
				Provider: provider,
				Events:   events,
				Tools:    []Tool{nil},
			},
			wantError: "Tools[0] is nil",
		},
		{
			name: "empty tool name rejected",
			opts: Options{
				Provider: provider,
				Events:   events,
				Tools:    []Tool{namedTool{name: ""}},
			},
			wantError: "empty Definition().Function.Name",
		},
		{
			name: "duplicate tool name rejected",
			opts: Options{
				Provider: provider,
				Events:   events,
				Tools: []Tool{
					namedTool{name: "read_file"},
					namedTool{name: "read_file"},
				},
			},
			wantError: `duplicate tool name "read_file"`,
		},
		{
			name: "valid options succeed",
			opts: Options{
				Provider: provider,
				Events:   events,
				Tools: []Tool{
					namedTool{name: "read_file"},
					namedTool{name: "edit_file"},
				},
			},
			wantError: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag, err := New(tc.opts)

			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("New(%s): unexpected error: %v", tc.name, err)
				}
				if ag == nil {
					t.Fatalf("New(%s): nil agent on success", tc.name)
				}
				if len(ag.tools) != len(tc.opts.Tools) {
					t.Errorf("tools map size = %d, want %d", len(ag.tools), len(tc.opts.Tools))
				}
				if len(ag.toolDefs) != len(tc.opts.Tools) {
					t.Errorf("toolDefs slice size = %d, want %d", len(ag.toolDefs), len(tc.opts.Tools))
				}
				return
			}

			if err == nil {
				t.Fatalf("New(%s): expected error %q, got nil", tc.name, tc.wantError)
			}
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("New(%s): error %v does not wrap ErrInvalidOptions", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("New(%s): error %q missing reason %q", tc.name, err.Error(), tc.wantError)
			}
			if ag != nil {
				t.Errorf("New(%s): non-nil agent on error", tc.name)
			}
		})
	}
}
