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
//
// Parses go.mod structure semantically: only matches module paths inside
// require directives (both single-line and block form). Ignores module,
// go, replace, exclude, retract directives and comments.
func (t *PackageInfoTool) searchGoMod(modFile, pkg string) (modDir, version string) {
	content, err := os.ReadFile(modFile)
	if err != nil {
		return "", ""
	}

	dir := filepath.Dir(modFile)
	inRequireBlock := false

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)

		// Skip comments and empty lines.
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		// Track require block boundaries.
		if line == ")" {
			inRequireBlock = false
			continue
		}
		if strings.HasPrefix(line, "require (") || line == "require (" {
			inRequireBlock = true
			continue
		}

		// Single-line require: "require module/path v1.2.3"
		if strings.HasPrefix(line, "require ") && !strings.Contains(line, "(") {
			line = strings.TrimPrefix(line, "require ")
			line = strings.TrimSpace(line)
			if mod, ver := matchRequireLine(line, pkg); mod != "" {
				return dir, ver
			}
			continue
		}

		// Inside a require block: "module/path v1.2.3"
		if inRequireBlock {
			if mod, ver := matchRequireLine(line, pkg); mod != "" {
				return dir, ver
			}
		}
	}
	return "", ""
}

// matchRequireLine checks if a require entry (e.g. "module/path v1.2.3")
// matches the given package. Returns (module, version) on match, or ("", "").
// Matches if the entry's module path equals pkg or pkg is a subpackage.
func matchRequireLine(line, pkg string) (mod, version string) {
	// Strip inline comments.
	if idx := strings.Index(line, "//"); idx >= 0 {
		line = line[:idx]
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", ""
	}
	mod, version = parts[0], parts[1]
	if mod == pkg || strings.HasPrefix(pkg, mod+"/") {
		return mod, version
	}
	return "", ""
}

// maxGoDocOutput caps the output from go doc to prevent unbounded memory use.
// Matches maxBashOutput for consistency across subprocess-executing tools.
const maxGoDocOutput = 8 * 1024

// runGoDoc executes "go doc" and returns the output.
// Uses limitedWriter to cap output during execution, consistent with BashTool.
func (t *PackageInfoTool) runGoDoc(ctx context.Context, modDir, target string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, packageInfoTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "go", "doc", target)
	cmd.Dir = modDir

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutLW := &limitedWriter{w: &stdoutBuf, remaining: maxGoDocOutput}
	stderrLW := &limitedWriter{w: &stderrBuf, remaining: maxGoDocOutput}
	cmd.Stdout = stdoutLW
	cmd.Stderr = stderrLW

	if err := cmd.Run(); err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return "Error: go doc timed out"
		}
		if stderrBuf.Len() > 0 {
			return fmt.Sprintf("go doc error: %s", strings.TrimSpace(stderrBuf.String()))
		}
		return fmt.Sprintf("go doc error: %v", err)
	}

	output := stdoutBuf.String()
	if stdoutLW.truncated {
		output += "\n[... output truncated]"
	}
	return output
}
