package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

// globWorkspace is a Workspace stub that returns a fixed file list.
type globWorkspace struct {
	testWorkspace
	fileList []string
}

func (w *globWorkspace) ListFiles() ([]string, error) {
	return w.fileList, nil
}

type errListWorkspace struct {
	testWorkspace
}

func (w *errListWorkspace) ListFiles() ([]string, error) {
	return nil, fmt.Errorf("disk on fire")
}

func makeGlobCall(t *testing.T, args globArgs) llm.ToolCall {
	t.Helper()
	return llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "glob", Arguments: string(mustMarshal(t, args))},
	}
}

// projectFiles is the shared file tree for glob matching tests.
var projectFiles = []string{
	"README.md",
	"go.mod",
	"engine/agent/agent.go",
	"engine/agent/tool.go",
	"engine/agent/tool_glob.go",
	"engine/agent/tool_glob_test.go",
	"engine/agent/tool_read_file.go",
	"engine/agent/tool_read_file_test.go",
	"engine/buffer/buffer.go",
	"engine/buffer/buffer_test.go",
	"engine/filelist/filelist.go",
	"engine/filelist/gitignore.go",
	"tui/internal/ui/app.go",
	"tui/internal/ui/editor.go",
	"tui/cmd/junto/main.go",
}

type globTestCase struct {
	name         string
	args         globArgs
	files        []string
	wantErr      string
	wantContains []string
	wantAbsent   []string
	wantCount    string
}

func runGlobTests(t *testing.T, tests []globTestCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := tt.files
			if files == nil {
				files = projectFiles
			}
			ws := &globWorkspace{fileList: files}
			tool := NewGlobTool(ws)
			result := tool.Execute(context.Background(), makeGlobCall(t, tt.args))

			if tt.wantErr != "" {
				if !strings.Contains(result.Content, tt.wantErr) {
					t.Errorf("expected error containing %q, got:\n%s", tt.wantErr, result.Content)
				}
				return
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(result.Content, want) {
					t.Errorf("expected result to contain %q, got:\n%s", want, result.Content)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(result.Content, absent) {
					t.Errorf("expected result NOT to contain %q, got:\n%s", absent, result.Content)
				}
			}
			if tt.wantCount != "" && !strings.Contains(result.Content, tt.wantCount) {
				t.Errorf("expected count %q in result, got:\n%s", tt.wantCount, result.Content)
			}
		})
	}
}

func TestGlobTool_Patterns(t *testing.T) {
	runGlobTests(t, []globTestCase{
		{
			name:    "empty pattern",
			args:    globArgs{Pattern: ""},
			wantErr: "pattern is required",
		},
		{
			name:         "match all Go files with double star",
			args:         globArgs{Pattern: "**/*.go"},
			wantContains: []string{"engine/agent/agent.go", "tui/cmd/junto/main.go"},
			wantAbsent:   []string{"README.md", "go.mod"},
			wantCount:    "13 file(s) found.",
		},
		{
			name:         "match test files",
			args:         globArgs{Pattern: "**/*_test.go"},
			wantContains: []string{"engine/agent/tool_glob_test.go", "engine/buffer/buffer_test.go"},
			wantAbsent:   []string{"engine/agent/agent.go"},
			wantCount:    "3 file(s) found.",
		},
		{
			name:         "match by extension in root",
			args:         globArgs{Pattern: "*.md"},
			wantContains: []string{"README.md"},
			wantAbsent:   []string{"go.mod"},
			wantCount:    "1 file(s) found.",
		},
		{
			name:    "no matches",
			args:    globArgs{Pattern: "**/*.rs"},
			wantErr: "No files found matching pattern",
		},
	})
}

func TestGlobTool_PathScoping(t *testing.T) {
	runGlobTests(t, []globTestCase{
		{
			name:         "only engine/agent",
			args:         globArgs{Pattern: "**/*.go", Path: "engine/agent"},
			wantContains: []string{"engine/agent/agent.go", "engine/agent/tool_glob.go"},
			wantAbsent:   []string{"engine/buffer/buffer.go", "tui/cmd/junto/main.go"},
		},
		{
			name:         "trailing slash normalized",
			args:         globArgs{Pattern: "**/*.go", Path: "engine/agent/"},
			wantContains: []string{"engine/agent/agent.go"},
			wantAbsent:   []string{"engine/buffer/buffer.go"},
		},
		{
			name:         "basename pattern with path scope",
			args:         globArgs{Pattern: "*.go", Path: "engine/agent"},
			wantContains: []string{"engine/agent/agent.go", "engine/agent/tool_glob.go"},
			wantAbsent:   []string{"engine/buffer/buffer.go"},
		},
		{
			name:         "leading slash normalized",
			args:         globArgs{Pattern: "*.go", Path: "/engine/agent"},
			wantContains: []string{"engine/agent/agent.go", "engine/agent/tool_glob.go"},
			wantAbsent:   []string{"engine/buffer/buffer.go"},
		},
		{
			name:         "dot slash normalized",
			args:         globArgs{Pattern: "*.go", Path: "./engine/agent"},
			wantContains: []string{"engine/agent/agent.go", "engine/agent/tool_glob.go"},
			wantAbsent:   []string{"engine/buffer/buffer.go"},
		},
		{
			name:         "double trailing slash normalized",
			args:         globArgs{Pattern: "*.go", Path: "engine/agent//"},
			wantContains: []string{"engine/agent/agent.go", "engine/agent/tool_glob.go"},
			wantAbsent:   []string{"engine/buffer/buffer.go"},
		},
		{
			name:         "single star matches one segment",
			args:         globArgs{Pattern: "engine/*/buffer.go"},
			wantContains: []string{"engine/buffer/buffer.go"},
			wantAbsent:   []string{"engine/agent/agent.go"},
			wantCount:    "1 file(s) found.",
		},
		{
			name:         "question mark wildcard",
			args:         globArgs{Pattern: "**/*.g?"},
			wantContains: []string{"engine/agent/agent.go"},
			wantAbsent:   []string{"README.md"},
		},
	})
}

func TestGlobTool_Truncation(t *testing.T) {
	var files []string
	for i := range maxGlobResults + 50 {
		files = append(files, fmt.Sprintf("src/file_%04d.go", i))
	}
	sort.Strings(files)

	ws := &globWorkspace{fileList: files}
	tool := NewGlobTool(ws)
	result := tool.Execute(context.Background(), makeGlobCall(t, globArgs{Pattern: "**/*.go"}))

	if !strings.Contains(result.Content, "showing first 100") {
		t.Errorf("expected truncation notice, got:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "150 files matched") {
		t.Errorf("expected total count of 150, got:\n%s", result.Content)
	}

	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	fileLines := 0
	for _, line := range lines {
		if line != "" && !strings.HasPrefix(line, "(") {
			fileLines++
		}
	}
	if fileLines != maxGlobResults {
		t.Errorf("expected %d file lines, got %d", maxGlobResults, fileLines)
	}
}

func TestGlobTool_ListFilesError(t *testing.T) {
	ws := &errListWorkspace{}
	tool := NewGlobTool(ws)
	result := tool.Execute(context.Background(), makeGlobCall(t, globArgs{Pattern: "**/*.go"}))

	if !strings.Contains(result.Content, "disk on fire") {
		t.Errorf("expected ListFiles error propagated, got:\n%s", result.Content)
	}
}

func TestGlobTool_InvalidJSON(t *testing.T) {
	ws := &globWorkspace{}
	tool := NewGlobTool(ws)
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "glob", Arguments: "not json"},
	}
	result := tool.Execute(context.Background(), call)

	if !strings.Contains(result.Content, "invalid arguments") {
		t.Errorf("expected invalid arguments error, got:\n%s", result.Content)
	}
}

func TestGlobTool_Definition(t *testing.T) {
	ws := &globWorkspace{}
	tool := NewGlobTool(ws)
	def := tool.Definition()

	if def.Function.Name != "glob" {
		t.Errorf("expected tool name 'glob', got %q", def.Function.Name)
	}
	if _, ok := def.Function.Parameters.Properties["pattern"]; !ok {
		t.Error("expected 'pattern' in parameters")
	}
	if _, ok := def.Function.Parameters.Properties["path"]; !ok {
		t.Error("expected 'path' in parameters")
	}
	if len(def.Function.Parameters.Required) != 1 || def.Function.Parameters.Required[0] != "pattern" {
		t.Errorf("expected Required=[pattern], got %v", def.Function.Parameters.Required)
	}
}
