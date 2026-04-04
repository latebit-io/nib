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

//go:embed prompts/system.md prompts/planning_system.md prompts/user.md.tmpl
var defaultPrompts embed.FS

// maxPromptFileBytes is the size limit for project prompt overrides (1MB).
// No sane prompt file should approach this; protects against accidental
// large files in .project/prompts/.
const maxPromptFileBytes = 1 << 20

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

// SystemPrompt returns the system prompt text.
func (l *PromptLoader) SystemPrompt() string {
	content, source := l.load("system.md")
	slog.Debug("prompt.SystemPrompt", "source", source)
	return strings.TrimSpace(content)
}

// PlanningSystemPrompt returns the planning-mode system prompt text.
func (l *PromptLoader) PlanningSystemPrompt() string {
	content, source := l.load("planning_system.md")
	slog.Debug("prompt.PlanningSystemPrompt", "source", source)
	return strings.TrimSpace(content)
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
