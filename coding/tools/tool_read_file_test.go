package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/tools/truncate"
)

// errWorkspace returns an error for files not in its map, unlike testWorkspace
// which returns ("", nil). This lets us test the error propagation path.
type errWorkspace struct {
	testWorkspace
}

func (w *errWorkspace) ReadFile(path string) (string, error) {
	if c, ok := w.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("no such file: %s", path)
}

func newReadTestWorkspace(files map[string]string) *testWorkspace {
	return &testWorkspace{
		files:     files,
		inContext: map[string]bool{},
	}
}

func makeReadCall(t *testing.T, args readArgs) llm.ToolCall {
	t.Helper()
	return llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "read_file", Arguments: string(mustMarshal(t, args))},
	}
}

type readTestCase struct {
	name         string
	content      string
	args         readArgs
	wantErr      string
	wantExact    string
	wantContains []string
	wantAbsent   []string
}

func runReadTests(t *testing.T, tests []readTestCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{}
			if tt.args.Path != "" {
				files[tt.args.Path] = tt.content
			}
			ws := newReadTestWorkspace(files)
			tool := NewReadFileTool(ws, NewFileCache())
			result := tool.Execute(context.Background(), makeReadCall(t, tt.args))
			assertReadResult(t, result, tt.wantErr, tt.wantExact, tt.wantContains, tt.wantAbsent)
		})
	}
}

func assertReadResult(t *testing.T, result ToolResult, wantErr, wantExact string, wantContains, wantAbsent []string) {
	t.Helper()
	if wantErr != "" {
		if !strings.Contains(result.Content, wantErr) {
			t.Errorf("expected error containing %q, got %q", wantErr, result.Content)
		}
		return
	}
	if wantExact != "" {
		if result.Content != wantExact {
			t.Errorf("content mismatch:\n got:  %q\n want: %q", result.Content, wantExact)
		}
		return
	}
	for _, want := range wantContains {
		if !strings.Contains(result.Content, want) {
			t.Errorf("expected content to contain %q, got:\n%s", want, result.Content)
		}
	}
	for _, absent := range wantAbsent {
		if strings.Contains(result.Content, absent) {
			t.Errorf("expected content NOT to contain %q, got:\n%s", absent, result.Content)
		}
	}
}

func TestReadFileTool_FullFile(t *testing.T) {
	runReadTests(t, []readTestCase{
		{
			name:      "returns raw content without line numbers",
			content:   "package main\n\nfunc main() {}",
			args:      readArgs{Path: "main.go"},
			wantExact: "package main\n\nfunc main() {}",
		},
		{
			name:    "missing path returns error",
			content: "anything",
			args:    readArgs{Path: ""},
			wantErr: "path is required",
		},
		{
			name:    "negative offset returns error",
			content: "content",
			args:    readArgs{Path: "main.go", Offset: -1},
			wantErr: "offset and limit must be non-negative",
		},
		{
			name:    "negative limit returns error",
			content: "content",
			args:    readArgs{Path: "main.go", Limit: -5},
			wantErr: "offset and limit must be non-negative",
		},
	})
}

func TestReadFileTool_LineSlicing(t *testing.T) {
	tenLines := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10"

	runReadTests(t, []readTestCase{
		{
			name:         "first 3 lines",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Offset: 1, Limit: 3},
			wantContains: []string{"Lines 1–3 of 10", "   1\tline1", "   2\tline2", "   3\tline3"},
			wantAbsent:   []string{"line4"},
		},
		{
			name:         "middle slice",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Offset: 4, Limit: 3},
			wantContains: []string{"Lines 4–6 of 10", "   4\tline4", "   5\tline5", "   6\tline6"},
			wantAbsent:   []string{"line3", "line7"},
		},
		{
			name:    "offset past end of file",
			content: tenLines,
			args:    readArgs{Path: "main.go", Offset: 999},
			wantErr: "offset 999 exceeds file length (10 lines)",
		},
		{
			name:         "offset at last line reads to end",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Offset: 10},
			wantContains: []string{"Lines 10–10 of 10", "  10\tline10"},
			wantAbsent:   []string{"line9"},
		},
		{
			name:         "limit exceeds remaining lines clamps to end",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Offset: 8, Limit: 999},
			wantContains: []string{"Lines 8–10 of 10", "   8\tline8", "  10\tline10"},
		},
		{
			name:         "offset zero treated as offset one",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Offset: 0, Limit: 2},
			wantContains: []string{"Lines 1–2 of 10", "   1\tline1", "   2\tline2"},
			wantAbsent:   []string{"line3"},
		},
		{
			name:         "limit only without offset reads from start",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Limit: 2},
			wantContains: []string{"Lines 1–2 of 10", "   1\tline1", "   2\tline2"},
			wantAbsent:   []string{"line3"},
		},
		{
			name:         "offset only without limit reads to end",
			content:      tenLines,
			args:         readArgs{Path: "main.go", Offset: 9},
			wantContains: []string{"Lines 9–10 of 10", "   9\tline9", "  10\tline10"},
			wantAbsent:   []string{"line8"},
		},
		{
			name:         "single line file with slice",
			content:      "only line",
			args:         readArgs{Path: "main.go", Offset: 1, Limit: 5},
			wantContains: []string{"Lines 1–1 of 1", "   1\tonly line"},
		},
	})
}

func TestReadFileTool_CacheHitWithSlice(t *testing.T) {
	ws := newReadTestWorkspace(map[string]string{"main.go": "a\nb\nc\nd\ne"})
	tool := NewReadFileTool(ws, NewFileCache())

	// First call populates cache (full read).
	r1 := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "main.go"}))
	if r1.Content != "a\nb\nc\nd\ne" {
		t.Fatalf("unexpected full read: %q", r1.Content)
	}

	// Second call with offset/limit should use cached content.
	r2 := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "main.go", Offset: 2, Limit: 2}))
	if !strings.Contains(r2.Content, "Lines 2–3 of 5") {
		t.Errorf("expected slice from cache, got:\n%s", r2.Content)
	}
	if !strings.Contains(r2.Content, "   2\tb") || !strings.Contains(r2.Content, "   3\tc") {
		t.Errorf("expected lines 2-3, got:\n%s", r2.Content)
	}
}

func TestReadFileTool_FileTooLarge(t *testing.T) {
	huge := strings.Repeat("x", maxFileSize+1)
	ws := newReadTestWorkspace(map[string]string{"big.bin": huge})
	tool := NewReadFileTool(ws, NewFileCache())

	result := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "big.bin"}))
	if !strings.Contains(result.Content, "file too large") {
		t.Errorf("expected size error, got %q", result.Content)
	}
}

func TestReadFileTool_FileTooLargeCacheHit(t *testing.T) {
	// Oversized content can enter the cache via cache.Reset() (agent startup)
	// or cache.Set() (edit_file post-apply). Verify the cache-hit path rejects it.
	ws := newReadTestWorkspace(map[string]string{})
	cache := NewFileCache()
	cache.Set("big.bin", strings.Repeat("x", maxFileSize+1))
	tool := NewReadFileTool(ws, cache)

	result := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "big.bin"}))
	if !strings.Contains(result.Content, "file too large") {
		t.Errorf("expected size error on cache hit, got %q", result.Content)
	}
}

// recordingStash is a [truncate.Sink] that captures stashed payloads
// in-memory so tests can verify the full pre-truncation content was
// handed off without touching disk.
type recordingStash struct {
	calls    int
	lastBody string
}

func (r *recordingStash) Stash(label, content string) (string, error) {
	r.calls++
	r.lastBody = content
	return ".project/tooltmp/test/" + label + ".txt", nil
}

func TestReadFileTool_FullFileTruncation(t *testing.T) {
	// 100 KiB file blows past the 50 KiB default cap and must
	// truncate with a marker. With a sink attached, the marker
	// includes the stash path and the sink sees the full content.
	big := strings.Repeat("a", 100*1024)
	ws := newReadTestWorkspace(map[string]string{"big.log": big})
	tool := NewReadFileTool(ws, NewFileCache())
	sink := &recordingStash{}
	tool.SetStash(sink)

	result := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "big.log"}))

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if len(result.Content) > truncate.DefaultMaxBytes+256 {
		t.Fatalf("result %d bytes exceeds cap+marker budget", len(result.Content))
	}
	if !strings.Contains(result.Content, "[Truncated: showing") {
		t.Fatalf("missing marker, tail: %q", result.Content[max(0, len(result.Content)-200):])
	}
	if !strings.Contains(result.Content, "Full output: .project/tooltmp/") {
		t.Fatalf("marker missing Full-output path: tail: %q", result.Content[max(0, len(result.Content)-200):])
	}
	if sink.calls != 1 {
		t.Fatalf("expected sink invoked once, got %d", sink.calls)
	}
	if sink.lastBody != big {
		t.Fatalf("sink received %d bytes, want full %d", len(sink.lastBody), len(big))
	}
}

func TestReadFileTool_FullFileUnderCap(t *testing.T) {
	// A file under the cap must pass through untouched even with a
	// stash attached (CP2 — under-cap = passthrough).
	small := strings.Repeat("b", 1024)
	ws := newReadTestWorkspace(map[string]string{"small.log": small})
	tool := NewReadFileTool(ws, NewFileCache())
	sink := &recordingStash{}
	tool.SetStash(sink)

	result := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "small.log"}))
	if result.Content != small {
		t.Fatalf("expected passthrough, got len=%d", len(result.Content))
	}
	if sink.calls != 0 {
		t.Fatalf("expected sink untouched, got %d calls", sink.calls)
	}
}

func TestReadFileTool_SliceTruncation(t *testing.T) {
	// Build a file whose sliced output exceeds maxContentPreview.
	var lines []string
	for i := range 2000 {
		lines = append(lines, fmt.Sprintf("line content that is fairly long to fill up the buffer quickly %d", i))
	}
	bigContent := strings.Join(lines, "\n")

	ws := newReadTestWorkspace(map[string]string{"big.go": bigContent})
	tool := NewReadFileTool(ws, NewFileCache())

	result := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "big.go", Offset: 1, Limit: 2000}))
	if !strings.Contains(result.Content, "truncated") {
		t.Error("expected truncation notice in output")
	}
	if len(result.Content) > maxContentPreview+200 {
		t.Errorf("output too large after truncation: %d bytes", len(result.Content))
	}
}

func TestReadFileTool_FileNotFound(t *testing.T) {
	ws := &errWorkspace{testWorkspace{files: map[string]string{}, inContext: map[string]bool{}}}
	tool := NewReadFileTool(ws, NewFileCache())

	result := tool.Execute(context.Background(), makeReadCall(t, readArgs{Path: "nonexistent.go"}))
	if !strings.Contains(result.Content, "no such file") {
		t.Errorf("expected file-not-found error, got %q", result.Content)
	}
}

func TestReadFileTool_EdgeCases(t *testing.T) {
	runReadTests(t, []readTestCase{
		{
			name:    "empty file sliced returns single empty line",
			content: "",
			args:    readArgs{Path: "empty.go", Offset: 1, Limit: 5},
			// strings.Split("", "\n") → [""] → 1 line
			wantContains: []string{"Lines 1–1 of 1", "   1\t"},
		},
		{
			name:      "empty file full read returns empty string",
			content:   "",
			args:      readArgs{Path: "empty.go"},
			wantExact: "",
		},
		{
			name:    "trailing newline inflates line count",
			content: "a\nb\n",
			args:    readArgs{Path: "main.go", Offset: 1, Limit: 10},
			// strings.Split("a\nb\n", "\n") → ["a", "b", ""] → 3 lines
			wantContains: []string{"Lines 1–3 of 3", "   1\ta", "   2\tb", "   3\t\n"},
		},
		{
			name:    "no trailing newline correct line count",
			content: "a\nb",
			args:    readArgs{Path: "main.go", Offset: 1, Limit: 10},
			// strings.Split("a\nb", "\n") → ["a", "b"] → 2 lines
			wantContains: []string{"Lines 1–2 of 2", "   1\ta", "   2\tb"},
		},
	})
}
