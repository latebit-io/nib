package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

type mockNavigator struct {
	path string
	line int
	err  error
}

func (m *mockNavigator) GoToLine(path string, line int) error {
	m.path = path
	m.line = line
	return m.err
}

func TestGoToLineTool(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		navErr  error
		wantErr bool
		wantMsg string
	}{
		{
			name:    "valid navigation",
			args:    `{"path":"main.go","line":42}`,
			wantMsg: "Navigated to main.go line 42.",
		},
		{
			name:    "missing path",
			args:    `{"line":10}`,
			wantErr: true,
			wantMsg: "Error: path is required",
		},
		{
			name:    "line below 1",
			args:    `{"path":"main.go","line":0}`,
			wantErr: true,
			wantMsg: "Error: line must be >= 1",
		},
		{
			name:    "invalid json",
			args:    `not json`,
			wantErr: true,
			wantMsg: "Error: invalid arguments:",
		},
		{
			name:    "navigator error",
			args:    `{"path":"missing.go","line":1}`,
			navErr:  fmt.Errorf("file not found"),
			wantErr: true,
			wantMsg: "Error: file not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nav := &mockNavigator{err: tt.navErr}
			tool := NewGoToLineTool(nav)

			call := llm.ToolCall{
				ID:   "test",
				Type: "function",
				Function: llm.FunctionCall{
					Name:      "go_to_line",
					Arguments: tt.args,
				},
			}

			result := tool.Execute(context.Background(), call)

			if tt.wantErr {
				if len(result) < 5 || result[:5] != "Error" {
					// Check contains for partial match
					if result != tt.wantMsg && !contains(result, tt.wantMsg) {
						t.Errorf("got %q, want error containing %q", result, tt.wantMsg)
					}
				}
			} else {
				if result != tt.wantMsg {
					t.Errorf("got %q, want %q", result, tt.wantMsg)
				}
				if nav.path != "main.go" || nav.line != 42 {
					t.Errorf("navigator called with (%q, %d), want (main.go, 42)", nav.path, nav.line)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && searchSubstring(s, sub))
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
