package search

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
)

func call(args string) llm.ToolCall {
	return llm.ToolCall{
		Function: llm.FunctionCall{Arguments: args},
	}
}

func TestDefinition(t *testing.T) {
	tool := New("/tmp", func(context.Context, string, string, Options) ([]Result, error) { return nil, nil })
	def := tool.Definition()
	if def.Function.Name != "search_project" {
		t.Fatalf("name = %q, want search_project", def.Function.Name)
	}
	if _, ok := def.Function.Parameters.Properties["pattern"]; !ok {
		t.Fatal("missing pattern property")
	}
}

func TestExecute(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		fn         SearchFunc
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "invalid json",
			args:       `{bad`,
			fn:         func(context.Context, string, string, Options) ([]Result, error) { return nil, nil },
			wantErr:    true,
			wantSubstr: "invalid arguments",
		},
		{
			name:       "empty pattern",
			args:       `{"pattern":""}`,
			fn:         func(context.Context, string, string, Options) ([]Result, error) { return nil, nil },
			wantErr:    true,
			wantSubstr: "pattern is required",
		},
		{
			name: "search backend error",
			args: `{"pattern":"foo"}`,
			fn: func(context.Context, string, string, Options) ([]Result, error) {
				return nil, errors.New("rg not found")
			},
			wantErr:    true,
			wantSubstr: "rg not found",
		},
		{
			name: "no matches",
			args: `{"pattern":"nonexistent"}`,
			fn: func(context.Context, string, string, Options) ([]Result, error) {
				return nil, nil
			},
			wantSubstr: "No matches found",
		},
		{
			name: "results formatted",
			args: `{"pattern":"hello"}`,
			fn: func(context.Context, string, string, Options) ([]Result, error) {
				return []Result{
					{Path: "main.go", Line: 42, Text: `fmt.Println("hello")`},
					{Path: "lib.go", Line: 7, Text: `// hello world`},
				}, nil
			},
			wantSubstr: "2 match(es) found",
		},
		{
			name: "options forwarded",
			args: `{"pattern":"test","regex":true,"case_sensitive":true,"file_glob":"*.go"}`,
			fn: func(_ context.Context, _ string, pattern string, opts Options) ([]Result, error) {
				if pattern != "test" {
					return nil, errors.New("wrong pattern")
				}
				if !opts.Regex {
					return nil, errors.New("regex not forwarded")
				}
				if !opts.CaseSensitive {
					return nil, errors.New("case_sensitive not forwarded")
				}
				if opts.FileGlob != "*.go" {
					return nil, errors.New("file_glob not forwarded")
				}
				if opts.MaxResults != DefaultMaxResults {
					return nil, errors.New("max_results not set to default")
				}
				return []Result{{Path: "a.go", Line: 1, Text: "test"}}, nil
			},
			wantSubstr: "1 match(es) found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := New("/project", tt.fn)
			result := tool.Execute(context.Background(), call(tt.args))

			if tt.wantErr && !result.IsError {
				t.Errorf("expected IsError=true, got false; content=%q", result.Content)
			}
			if !tt.wantErr && result.IsError {
				t.Errorf("unexpected IsError=true; content=%q", result.Content)
			}
			if !strings.Contains(result.Content, tt.wantSubstr) {
				t.Errorf("content %q missing substring %q", result.Content, tt.wantSubstr)
			}
		})
	}
}

func TestExecute_Truncation(t *testing.T) {
	var results []Result
	for i := range 500 {
		results = append(results, Result{
			Path: "file.go",
			Line: i + 1,
			Text: strings.Repeat("x", 100),
		})
	}

	tool := New("/project", func(context.Context, string, string, Options) ([]Result, error) {
		return results, nil
	})
	result := tool.Execute(context.Background(), call(`{"pattern":"x"}`))

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if len(result.Content) > maxPreviewBytes+200 {
		t.Errorf("content not truncated: len=%d", len(result.Content))
	}
	if !strings.Contains(result.Content, "[... truncated") {
		t.Error("missing truncation marker")
	}
}

func TestExecute_ProjectRootForwarded(t *testing.T) {
	var gotRoot string
	tool := New("/my/project", func(_ context.Context, root string, _ string, _ Options) ([]Result, error) {
		gotRoot = root
		return nil, nil
	})
	tool.Execute(context.Background(), call(`{"pattern":"x"}`))

	if gotRoot != "/my/project" {
		t.Errorf("root = %q, want /my/project", gotRoot)
	}
}

func TestNew_NilSearchFuncPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil SearchFunc")
		}
	}()
	New("/tmp", nil)
}

// TestTool_SatisfiesContract verifies the search tool conforms to the
// [kit.Tool] contract when constructed with a benign no-op backend.
func TestTool_SatisfiesContract(t *testing.T) {
	contracttest.Tool(t, func() kit.Tool {
		return New("/tmp", func(context.Context, string, string, Options) ([]Result, error) {
			return nil, nil
		})
	})
}
