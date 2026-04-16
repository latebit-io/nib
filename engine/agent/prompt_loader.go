package agent

import (
	"bytes"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
)

//go:embed prompts/system.md.tmpl prompts/planning_system.md.tmpl prompts/user.md.tmpl
var defaultPrompts embed.FS

// maxPromptFileBytes is the size limit for project prompt overrides (1MB).
// No sane prompt file should approach this; protects against accidental
// large files in .project/prompts/.
const maxPromptFileBytes = 1 << 20

// CodingStyleData holds the style rules injected into the system prompt.
type CodingStyleData struct {
	// Name is the display name of the active coding style.
	Name string
	// Rules are the formatted rule strings (each is "**Name**: Instruction").
	Rules []string
}

// StyleRule is a single enforceable principle used to build CodingStyleData.
// Callers convert from their domain-specific rule types into this
// common form before passing to [NewCodingStyleData].
type StyleRule struct {
	// Name is a short label for the rule (e.g. "Single Responsibility").
	Name string
	// Instruction is the directive the agent must follow.
	Instruction string
	// Enforcement is "hard" (structural, verifiable — violations are rejected)
	// or "soft" (judgment-based — violations are flagged but not blocked).
	Enforcement string
}

// NewCodingStyleData creates a CodingStyleData from a name and a set of
// rules. Each rule is formatted with its enforcement level so the agent
// can distinguish mandatory constraints from advisory guidance.
func NewCodingStyleData(name string, rules []StyleRule) *CodingStyleData {
	formatted := make([]string, len(rules))
	for i, r := range rules {
		tag := "advisory"
		if r.Enforcement == "hard" {
			tag = "REQUIRED"
		}
		formatted[i] = "**" + r.Name + "** [" + tag + "]: " + r.Instruction
	}
	return &CodingStyleData{Name: name, Rules: formatted}
}

// SystemPromptData holds the template variables for system prompts.
type SystemPromptData struct {
	// Headless is true when the agent runs without a TUI (autonomous mode).
	// Controls prompt framing: approval flow vs direct edit application.
	Headless bool
	// Autonomous is true when the autonomy dial is set to trust mode.
	// Relaxes one-edit-at-a-time constraints so the agent works continuously.
	Autonomous bool
	// DistributedMemory lists MCP server names recognized as shared/team memory.
	// When non-empty, the template renders a section explaining local vs shared usage.
	DistributedMemory []string
	// CodingStyle holds the active style rules. Nil when no style is configured.
	CodingStyle *CodingStyleData
	// Terse enables terse output mode. When true, the template injects
	// instructions to minimize explanatory text, reducing output tokens.
	Terse bool
}

// UserPromptData holds the template variables for the user message.
type UserPromptData struct {
	// FileName is the active file's relative path.
	FileName string
	// FileContent is the file content with 1-indexed line numbers prepended.
	FileContent string
	// Fence is the code fence marker (``` or longer if the content contains backticks).
	Fence string
	// ContextFiles lists relative paths the agent is allowed to edit.
	ContextFiles []string
	// OmittedCount is how many context files were truncated from the prompt.
	OmittedCount int
	// Goal is the developer's stated intent for this agent run.
	Goal string
	// MemorySummary is the project memory summary injected on session start.
	// Empty string if memory is not configured.
	MemorySummary string
	// ActiveTaskPath is the ancestry path of the currently active task
	// from the project plan (e.g. "Phase 3 > Priority Field > Add field").
	// Empty if no task is active or no plan exists.
	ActiveTaskPath string
}

// PromptLoader resolves prompt files with project-level overrides.
// Load order: .project/prompts/<name> → embedded defaults.
type PromptLoader struct {
	projectRoot string

	// embeddedUserTmpl caches the compiled embedded user template.
	// The embedded template is immutable, so it only needs to be parsed once.
	embeddedUserTmpl *template.Template
	// embeddedUserErr persists a parse failure from the one-time embedded parse
	// so subsequent calls return the error instead of a nil template.
	embeddedUserErr error
	// embeddedUserOnce guards one-time parsing of the embedded user template.
	embeddedUserOnce sync.Once
}

// NewPromptLoader creates a loader that checks .project/prompts/ under
// projectRoot before falling back to embedded defaults. If projectRoot
// is empty, only embedded defaults are used.
func NewPromptLoader(projectRoot string) *PromptLoader {
	return &PromptLoader{projectRoot: projectRoot}
}

// SystemPrompt renders the execution-mode system prompt with the given data.
func (l *PromptLoader) SystemPrompt(data SystemPromptData) string {
	return l.renderSystemTemplate("system.md.tmpl", data)
}

// PlanningSystemPrompt renders the planning-mode system prompt with the given data.
func (l *PromptLoader) PlanningSystemPrompt(data SystemPromptData) string {
	return l.renderSystemTemplate("planning_system.md.tmpl", data)
}

// renderSystemTemplate loads a system prompt template by name and renders it
// with the given data. Project overrides are re-parsed on every call so edits
// during a session take effect immediately. If a project override fails to
// parse or execute, falls back to the embedded default template rather than
// returning raw (potentially broken) template syntax.
func (l *PromptLoader) renderSystemTemplate(name string, data SystemPromptData) string {
	raw, source := l.load(name)
	slog.Debug("prompt.renderSystemTemplate", "name", name, "source", source)

	out, err := renderTemplate(name, raw, data)
	if err == nil {
		return out
	}
	slog.Error("system prompt template render failed", "name", name, "source", source, "err", err)

	// Project override failed — fall back to the embedded default template.
	if source == "project" {
		embedded, readErr := defaultPrompts.ReadFile("prompts/" + name)
		if readErr != nil {
			// Embedded files are compiled in — this should never happen.
			panic(fmt.Sprintf("embedded prompt missing: %s: %v", name, readErr))
		}
		out, fallbackErr := renderTemplate(name, string(embedded), data)
		if fallbackErr == nil {
			return out
		}
		// Both project override and embedded default failed — truly broken binary.
		slog.Error("embedded prompt template also failed", "name", name, "err", fallbackErr)
	}

	// Last resort: return raw content (only reachable for broken embedded templates).
	return strings.TrimSpace(raw)
}

// renderTemplate parses and executes a Go template, returning the trimmed result.
func renderTemplate(name, raw string, data SystemPromptData) (string, error) {
	tmpl, err := template.New(name).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// RenderUserMessage executes the user message template with the given data.
// The embedded default template is compiled once and cached; project overrides
// from .project/prompts/ are re-parsed on every call so edits take effect
// immediately.
func (l *PromptLoader) RenderUserMessage(data UserPromptData) (string, error) {
	raw, source := l.load("user.md.tmpl")
	slog.Debug("prompt.RenderUserMessage", "source", source)

	tmpl, err := l.userTemplate(raw, source)
	if err != nil {
		return "", fmt.Errorf("parse user template (%s): %w", source, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute user template: %w", err)
	}
	return buf.String(), nil
}

// userTemplate returns a compiled template for the user message. Embedded
// templates are cached after first parse; project overrides are re-parsed
// every call so that edits during a session take effect immediately.
func (l *PromptLoader) userTemplate(raw, source string) (*template.Template, error) {
	if source == "embedded" {
		l.embeddedUserOnce.Do(func() {
			l.embeddedUserTmpl, l.embeddedUserErr = template.New("user").Parse(raw)
		})
		if l.embeddedUserErr != nil {
			return nil, l.embeddedUserErr
		}
		return l.embeddedUserTmpl, nil
	}

	return template.New("user").Parse(raw)
}

// load returns file content and its source ("project" or "embedded").
// Checks .project/prompts/<name> first, falls back to embedded.
func (l *PromptLoader) load(name string) (string, string) {
	if l.projectRoot != "" {
		path := filepath.Join(l.projectRoot, ".project", "prompts", name)
		info, err := os.Stat(path)
		if err == nil {
			if info.Size() > maxPromptFileBytes {
				slog.Warn("prompt.load: project override too large, using default",
					"path", path, "bytes", info.Size(), "limit", maxPromptFileBytes)
			} else {
				data, err := os.ReadFile(path)
				if err == nil {
					return string(data), "project"
				}
				slog.Warn("prompt.load: project override unreadable",
					"path", path, "err", err)
			}
		} else if !os.IsNotExist(err) {
			slog.Warn("prompt.load: project override stat failed",
				"path", path, "err", err)
		}
	}

	data, err := defaultPrompts.ReadFile("prompts/" + name)
	if err != nil {
		// Embedded files are compiled in — this means a corrupt binary.
		panic(fmt.Sprintf("embedded prompt missing: %s: %v", name, err))
	}
	return string(data), "embedded"
}
