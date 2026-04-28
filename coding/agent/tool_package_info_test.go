package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/ai/llm"
)

func pkgInfoCall(name, symbol string) llm.ToolCall {
	args := map[string]string{"name": name}
	if symbol != "" {
		args["symbol"] = symbol
	}
	data, _ := json.Marshal(args)
	return llm.ToolCall{
		ID: "test-pkg-1",
		Function: llm.FunctionCall{
			Name:      "package_info",
			Arguments: string(data),
		},
	}
}

func TestPackageInfoTool_Definition(t *testing.T) {
	tool := NewPackageInfoTool("/tmp")
	def := tool.Definition()

	if def.Function.Name != "package_info" {
		t.Errorf("expected tool name 'package_info', got %q", def.Function.Name)
	}
	if _, ok := def.Function.Parameters.Properties["name"]; !ok {
		t.Error("expected 'name' parameter in definition")
	}
	if _, ok := def.Function.Parameters.Properties["symbol"]; !ok {
		t.Error("expected 'symbol' parameter in definition")
	}
}

func TestPackageInfoTool_EmptyName(t *testing.T) {
	tool := NewPackageInfoTool("/tmp")
	result := tool.Execute(context.Background(), pkgInfoCall("", ""))
	if !strings.Contains(result.Content, "Error: name is required") {
		t.Errorf("expected name required error, got %q", result.Content)
	}
}

func TestPackageInfoTool_InvalidArgs(t *testing.T) {
	tool := NewPackageInfoTool("/tmp")
	result := tool.Execute(context.Background(), llm.ToolCall{
		ID: "test-1",
		Function: llm.FunctionCall{
			Name:      "package_info",
			Arguments: `{invalid`,
		},
	})
	if !strings.Contains(result.Content, "Error: invalid arguments") {
		t.Errorf("expected invalid arguments error, got %q", result.Content)
	}
}

func TestPackageInfoTool_NoManifest(t *testing.T) {
	dir := t.TempDir()
	tool := NewPackageInfoTool(dir)
	result := tool.Execute(context.Background(), pkgInfoCall("fmt", ""))
	if !strings.Contains(result.Content, "no supported package manager") {
		t.Errorf("expected no package manager error, got %q", result.Content)
	}
}

func TestPackageInfoTool_DetectLanguageRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewPackageInfoTool(dir)
	if lang := tool.detectLanguage(); lang != "go" {
		t.Errorf("expected 'go', got %q", lang)
	}
}

func TestPackageInfoTool_DetectLanguageSubdir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "submod")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module test/sub\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewPackageInfoTool(dir)
	if lang := tool.detectLanguage(); lang != "go" {
		t.Errorf("expected 'go', got %q", lang)
	}
}

func TestPackageInfoTool_FindGoModule(t *testing.T) {
	dir := t.TempDir()
	gomod := `module example.com/myproject

go 1.22

require (
	github.com/stretchr/testify v1.9.0
	charm.land/bubbletea/v2 v2.0.2
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewPackageInfoTool(dir)

	tests := []struct {
		name    string
		pkg     string
		wantDir bool
		wantVer string
	}{
		{"exact match", "charm.land/bubbletea/v2", true, "v2.0.2"},
		{"exact match other", "github.com/stretchr/testify", true, "v1.9.0"},
		{"subpackage match", "charm.land/bubbletea/v2/tea", true, "v2.0.2"},
		{"not found", "github.com/nonexistent/pkg", false, ""},
		{"go directive not matched", "go", false, ""},
		{"require keyword not matched", "require", false, ""},
		{"module directive not matched", "module", false, ""},
		{"module path not matched", "example.com/myproject", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modDir, ver := tool.findGoModule(tt.pkg)
			if tt.wantDir && modDir == "" {
				t.Errorf("expected to find module dir for %q", tt.pkg)
			}
			if !tt.wantDir && modDir != "" {
				t.Errorf("expected no match for %q, got dir %q", tt.pkg, modDir)
			}
			if ver != tt.wantVer {
				t.Errorf("expected version %q, got %q", tt.wantVer, ver)
			}
		})
	}
}

func TestPackageInfoTool_FindGoModuleSingleLineRequire(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/proj\n\ngo 1.22\n\nrequire github.com/pkg/errors v0.9.1\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewPackageInfoTool(dir)
	modDir, ver := tool.findGoModule("github.com/pkg/errors")
	if modDir == "" {
		t.Error("expected to find single-line require")
	}
	if ver != "v0.9.1" {
		t.Errorf("expected v0.9.1, got %q", ver)
	}
}

func TestPackageInfoTool_FindGoModuleMonorepo(t *testing.T) {
	dir := t.TempDir()

	// Root module — no bubbletea
	rootMod := "module example.com/engine\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(rootMod), 0644); err != nil {
		t.Fatal(err)
	}

	// Sub module — has bubbletea
	sub := filepath.Join(dir, "tui")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	subMod := "module example.com/tui\n\ngo 1.22\n\nrequire charm.land/bubbletea/v2 v2.0.2\n"
	if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte(subMod), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewPackageInfoTool(dir)
	modDir, ver := tool.findGoModule("charm.land/bubbletea/v2")

	if modDir != sub {
		t.Errorf("expected module dir %q, got %q", sub, modDir)
	}
	if ver != "v2.0.2" {
		t.Errorf("expected version v2.0.2, got %q", ver)
	}
}

func TestPackageInfoTool_PackageNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewPackageInfoTool(dir)
	result := tool.Execute(context.Background(), pkgInfoCall("github.com/nonexistent/pkg", ""))
	if !strings.Contains(result.Content, "not found") {
		t.Errorf("expected not found message, got %q", result.Content)
	}
}

func TestPackageInfoTool_StdlibPackage(t *testing.T) {
	// go doc works for stdlib packages without them being in go.mod.
	// The tool should find the go.mod (for the module dir) and go doc should work.
	dir := t.TempDir()
	gomod := "module test\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewPackageInfoTool(dir)

	// Stdlib packages won't be found in go.mod requires, so the tool
	// won't find a module for them. This is expected — the agent should
	// already know stdlib APIs.
	result := tool.Execute(context.Background(), pkgInfoCall("fmt", ""))
	if !strings.Contains(result.Content, "not found") {
		t.Errorf("expected not found for stdlib, got %q", result.Content)
	}
}

func TestPackageInfoTool_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	gomod := "module test\n\ngo 1.22\n\nrequire fmt v0.0.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewPackageInfoTool(dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Run in a goroutine with a deadline so the test fails fast
	// instead of hanging if runGoDoc ever stops honoring ctx.Done().
	type execResult struct{ r ToolResult }
	ch := make(chan execResult, 1)
	go func() {
		ch <- execResult{tool.Execute(ctx, pkgInfoCall("fmt", ""))}
	}()

	select {
	case got := <-ch:
		if got.r.Content == "" {
			t.Error("expected non-empty result on cancelled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return within 2s on a cancelled context")
	}
}
