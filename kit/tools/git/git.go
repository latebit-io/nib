// Package git provides read-only git tools (status, diff, log) so a
// coding agent can inspect repository state without shelling out —
// structured arguments in, bounded porcelain output back, and no
// approval prompt because nothing here can mutate.
//
// Read-only is enforced by construction, not convention: each tool
// builds its own argv and execs `git` directly (no shell), caller-
// supplied refs and paths are validated and paths always sit behind
// `--`, so an argument can never become a flag and no mutating
// subcommand is reachable.
//
// Like the other internal built-in tool packages (bash, memory,
// search) this package deliberately does NOT import kit — it depends
// only on agent and ai/llm so foundation-only agents can use it too.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// gitTimeout bounds every git invocation. Reads are fast; the bound
// exists for pathological repos and hung object stores, not tuning.
const gitTimeout = 30 * time.Second

// Output caps: keep the head (most diffs/logs front-load the signal)
// plus a small tail so the model sees the shape of what was cut.
const (
	maxHeadBytes = 12 * 1024
	maxTailBytes = 2 * 1024
)

// defaultLogCount and maxLogCount bound git_log entries.
const (
	defaultLogCount = 10
	maxLogCount     = 50
)

// InRepo reports whether root is inside a git work tree. Composition
// roots use it to register the git tools only where they can work —
// a non-repo project should not carry dead tools in its prompt. Any
// failure (git missing, timeout, not a repo) reports false.
func InRepo(root string) bool {
	if root == "" {
		// An empty root would make exec fall back to the process cwd
		// and probe whatever repository the binary happens to run in.
		// Fail closed: no root, no git tools.
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("git: rev-parse --is-inside-work-tree failed; git tools disabled", "root", root, "err", err)
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// run execs git with the given argv under root and returns the
// combined trimmed output. Errors carry git's stderr so the LLM sees
// the actual reason (bad ref, not a repository, …).
func run(ctx context.Context, root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// validateArg rejects ref/path arguments that could be parsed as flags
// or smuggle control characters into the argv. Git validates the rest
// (a nonexistent ref is git's error to report, not ours).
func validateArg(kind, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("invalid %s %q: must not start with '-'", kind, v)
	}
	if strings.ContainsAny(v, "\x00\n") {
		return fmt.Errorf("invalid %s: control characters not permitted", kind)
	}
	return nil
}

// truncate caps s to head+tail with a marker naming how much was cut
// and how to narrow the query. Small outputs pass through untouched.
// Both cut points snap to rune starts so a multibyte sequence in the
// output (CJK path, emoji in a commit message) is never split into
// invalid UTF-8 on its way to the LLM.
func truncate(s string) string {
	if len(s) <= maxHeadBytes+maxTailBytes {
		return s
	}
	head := maxHeadBytes
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tailStart := len(s) - maxTailBytes
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return fmt.Sprintf("%s\n\n[... %d bytes truncated — narrow with a path or use stat/count ...]\n\n%s",
		s[:head], tailStart-head, s[tailStart:])
}

// result wraps command output as a successful ToolResult; errResult
// wraps a failure so the LLM can adapt (never a Go error, which would
// terminate the run).
func result(out string) agent.ToolResult {
	if out == "" {
		out = "(no output)"
	}
	return agent.ToolResult{Content: truncate(out)}
}

func errResult(err error) agent.ToolResult {
	return agent.ToolResult{Content: "Error: " + err.Error(), IsError: true}
}

// StatusTool reports working-tree state via porcelain status.
type StatusTool struct{ root string }

// NewStatusTool creates a git_status tool rooted at the project directory.
func NewStatusTool(root string) *StatusTool { return &StatusTool{root: root} }

// Definition returns the git_status schema.
func (t *StatusTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "git_status",
			Description: "Show git working-tree status: current branch, ahead/behind, and changed files in porcelain format. " +
				"Prefer this over `git status` via bash — it is structured and needs no approval.",
			Parameters: llm.FunctionParams{Type: "object", Properties: map[string]llm.FunctionParam{}},
		},
	}
}

// Execute runs porcelain status with the branch header.
func (t *StatusTool) Execute(ctx context.Context, _ llm.ToolCall) agent.ToolResult {
	out, err := run(ctx, t.root, "status", "--porcelain=v1", "--branch")
	if err != nil {
		return errResult(err)
	}
	if strings.Count(out, "\n") == 0 {
		// Only the "## branch" header: nothing changed. Say so rather
		// than returning a bare header the model may misread.
		out += "\n(working tree clean)"
	}
	return result(out)
}

// PromptGuidelines steers git reads toward the structured tools
// (kit.PromptContributor, satisfied structurally — this package cannot
// import kit). One bullet for the whole family, carried by the status
// tool since the three register together.
func (t *StatusTool) PromptGuidelines() []string {
	return []string{
		"Use `git_status` / `git_diff` / `git_log` for reading git state — not bash. They are structured, bounded, and run without an approval prompt.",
	}
}

// DiffTool shows working-tree or ref diffs.
type DiffTool struct{ root string }

// NewDiffTool creates a git_diff tool rooted at the project directory.
func NewDiffTool(root string) *DiffTool { return &DiffTool{root: root} }

// Definition returns the git_diff schema.
func (t *DiffTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "git_diff",
			Description: "Show a git diff. Defaults to unstaged working-tree changes; set staged=true for the index, " +
				"ref to diff against a commit/branch, path to scope to one file or directory, stat=true for a summary only. " +
				"Prefer this over `git diff` via bash.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"staged": {Type: "boolean", Description: "Diff the staged index instead of the working tree. Default: false."},
					"ref":    {Type: "string", Description: "Commit/branch to diff against (e.g. \"main\", \"HEAD~1\"). Optional."},
					"path":   {Type: "string", Description: "Limit the diff to a file or directory. Optional."},
					"stat":   {Type: "boolean", Description: "Show only the per-file change summary. Default: false."},
				},
			},
		},
	}
}

// Execute builds and runs the diff argv from validated arguments.
func (t *DiffTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	var args struct {
		Staged bool   `json:"staged"`
		Ref    string `json:"ref"`
		Path   string `json:"path"`
		Stat   bool   `json:"stat"`
	}
	// A zero-argument call can arrive with empty Arguments rather than
	// "{}"; every field is optional, so empty means all defaults.
	if raw := strings.TrimSpace(call.Function.Arguments); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return errResult(fmt.Errorf("invalid arguments: %w", err))
		}
	}
	argv := []string{"diff"}
	if args.Staged {
		argv = append(argv, "--cached")
	}
	if args.Stat {
		argv = append(argv, "--stat")
	}
	if args.Ref != "" {
		if err := validateArg("ref", args.Ref); err != nil {
			return errResult(err)
		}
		argv = append(argv, args.Ref)
	}
	if args.Path != "" {
		if err := validateArg("path", args.Path); err != nil {
			return errResult(err)
		}
		argv = append(argv, "--", args.Path)
	}
	out, err := run(ctx, t.root, argv...)
	if err != nil {
		return errResult(err)
	}
	if out == "" {
		out = "(no differences)"
	}
	return result(out)
}

// LogTool shows recent commit history.
type LogTool struct{ root string }

// NewLogTool creates a git_log tool rooted at the project directory.
func NewLogTool(root string) *LogTool { return &LogTool{root: root} }

// Definition returns the git_log schema.
func (t *LogTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "git_log",
			Description: "Show recent commit history, one line per commit with refs decorated. " +
				"Set count (default 10, max 50), ref to start from a commit/branch, path to scope to one file or directory. " +
				"Prefer this over `git log` via bash.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"count": {Type: "integer", Description: "Number of commits to show. Default 10, max 50."},
					"ref":   {Type: "string", Description: "Commit/branch to start from (e.g. \"main\"). Optional."},
					"path":  {Type: "string", Description: "Limit history to a file or directory. Optional."},
				},
			},
		},
	}
}

// Execute builds and runs the log argv from validated arguments.
func (t *LogTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	var args struct {
		Count int    `json:"count"`
		Ref   string `json:"ref"`
		Path  string `json:"path"`
	}
	// A zero-argument call can arrive with empty Arguments rather than
	// "{}"; every field is optional, so empty means all defaults.
	if raw := strings.TrimSpace(call.Function.Arguments); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return errResult(fmt.Errorf("invalid arguments: %w", err))
		}
	}
	count := args.Count
	if count <= 0 {
		count = defaultLogCount
	}
	if count > maxLogCount {
		count = maxLogCount
	}
	argv := []string{"log", "--oneline", "--decorate", "-n", strconv.Itoa(count)}
	if args.Ref != "" {
		if err := validateArg("ref", args.Ref); err != nil {
			return errResult(err)
		}
		argv = append(argv, args.Ref)
	}
	if args.Path != "" {
		if err := validateArg("path", args.Path); err != nil {
			return errResult(err)
		}
		argv = append(argv, "--", args.Path)
	}
	out, err := run(ctx, t.root, argv...)
	if err != nil {
		return errResult(err)
	}
	return result(out)
}
