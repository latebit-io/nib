package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit-io/junto/engine/llm"
)

// packageInfoTimeout is the max duration for package manager commands.
const packageInfoTimeout = 15 * time.Second

// PackageInfoTool lets the LLM look up package versions and API documentation.
// Prevents the agent from relying on stale training data when writing code
// that uses external libraries.
type PackageInfoTool struct {
	projectRoot string
}

// NewPackageInfoTool creates a PackageInfoTool rooted at the given project directory.
func NewPackageInfoTool(projectRoot string) *PackageInfoTool {
	return &PackageInfoTool{projectRoot: projectRoot}
}

// Definition returns the tool schema for the LLM.
func (t *PackageInfoTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "package_info",
			Description: "Look up installed package version and API documentation from the project's package manager. " +
				"Use this BEFORE writing code that imports external libraries to ensure you use correct, current APIs. " +
				"Supports Go modules (go.mod). Returns installed version and exported API surface.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"name": {
						Type:        "string",
						Description: "Package import path (e.g. \"charm.land/bubbletea/v2\", \"encoding/json\").",
					},
					"symbol": {
						Type:        "string",
						Description: "Specific symbol to look up (e.g. \"KeyPressMsg\", \"Model\"). Omit for package-level overview.",
					},
				},
				Required: []string{"name"},
			},
		},
	}
}

type packageInfoArgs struct {
	Name   string `json:"name"`
	Symbol string `json:"symbol"`
}

// Execute handles a package_info tool call.
func (t *PackageInfoTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	var args packageInfoArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Name == "" {
		return textResult("Error: name is required")
	}

	lang := t.detectLanguage()
	switch lang {
	case "go":
		return t.goPackageInfo(ctx, args)
	default:
		return textResult("Error: no supported package manager detected. " +
			"Looked for go.mod in project root and immediate subdirectories.")
	}
}

// detectLanguage checks for manifest files to determine the project language.
// Checks root and one level of subdirectories (for monorepos).
func (t *PackageInfoTool) detectLanguage() string {
	patterns := []string{"go.mod", "*/go.mod"}
	for _, pattern := range patterns {
		if matches, _ := filepath.Glob(filepath.Join(t.projectRoot, pattern)); len(matches) > 0 {
			return "go"
		}
	}
	return ""
}

// goPackageInfo looks up a Go package's installed version and documentation.
func (t *PackageInfoTool) goPackageInfo(ctx context.Context, args packageInfoArgs) ToolResult {
	modDir, version := t.findGoModule(args.Name)
	if modDir == "" {
		return textResult(fmt.Sprintf("Package %q not found in any go.mod in the project. "+
			"Check the import path is correct.", args.Name))
	}

	var result strings.Builder
	if version != "" {
		fmt.Fprintf(&result, "Installed version: %s\n\n", version)
	}

	target := args.Name
	if args.Symbol != "" {
		target = args.Name + "." + args.Symbol
	}
	doc := t.runGoDoc(ctx, modDir, target)
	result.WriteString(doc)

	out := result.String()
	if len(out) > maxContentPreview {
		out = out[:maxContentPreview] + "\n... (truncated)"
	}
	return textResult(out)
}

// findGoModule searches go.mod files for a dependency matching the given package.
// Returns the module directory and installed version, or empty strings if not found.
func (t *PackageInfoTool) findGoModule(pkg string) (modDir, version string) {
	patterns := []string{"go.mod", "*/go.mod"}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(t.projectRoot, pattern))
		if err != nil {
			continue
		}
		for _, modFile := range matches {
			dir, ver := t.searchGoMod(modFile, pkg)
			if dir != "" {
				return dir, ver
			}
		}
	}
	return "", ""
}

// searchGoMod reads a go.mod file and checks if pkg appears as a dependency.
// Also matches if pkg is a subpackage of a required module.
func (t *PackageInfoTool) searchGoMod(modFile, pkg string) (modDir, version string) {
	content, err := os.ReadFile(modFile)
	if err != nil {
		return "", ""
	}

	dir := filepath.Dir(modFile)

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") {
			continue
		}

		parts := strings.Fields(line)
		for i, p := range parts {
			// Exact module match or pkg is a subpackage of this module.
			if p == pkg || strings.HasPrefix(pkg, p+"/") {
				if i+1 < len(parts) && strings.HasPrefix(parts[i+1], "v") {
					return dir, parts[i+1]
				}
				return dir, ""
			}
		}
	}
	return "", ""
}

// runGoDoc executes "go doc" and returns the output.
func (t *PackageInfoTool) runGoDoc(ctx context.Context, modDir, target string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, packageInfoTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "go", "doc", target)
	cmd.Dir = modDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return "Error: go doc timed out"
		}
		if stderr.Len() > 0 {
			return fmt.Sprintf("go doc error: %s", strings.TrimSpace(stderr.String()))
		}
		return fmt.Sprintf("go doc error: %v", err)
	}
	return stdout.String()
}
