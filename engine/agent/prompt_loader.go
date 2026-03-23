package agent

import (
	"bytes"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed prompts/system.md prompts/user.md.tmpl
var defaultPrompts embed.FS

// UserPromptData holds the template variables for the user message.
type UserPromptData struct {
	FileName     string
	FileContent  string // line-numbered content
	Fence        string // code fence marker (``` or longer)
	ContextFiles []string
	OmittedCount int
	Goal         string
}

// PromptLoader resolves prompt files with project-level overrides.
// Load order: .project/prompts/<name> → embedded defaults.
type PromptLoader struct {
	projectRoot string
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

// RenderUserMessage executes the user message template with the given data.
func (l *PromptLoader) RenderUserMessage(data UserPromptData) (string, error) {
	raw, source := l.load("user.md.tmpl")
	slog.Debug("prompt.RenderUserMessage", "source", source)

	tmpl, err := template.New("user").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse user template (%s): %w", source, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute user template: %w", err)
	}
	return buf.String(), nil
}

// load returns file content and its source ("project" or "embedded").
// Checks .project/prompts/<name> first, falls back to embedded.
func (l *PromptLoader) load(name string) (string, string) {
	if l.projectRoot != "" {
		path := filepath.Join(l.projectRoot, ".project", "prompts", name)
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), "project"
		}
		// Not found or unreadable — fall through to embedded.
		if !os.IsNotExist(err) {
			slog.Warn("prompt.load: project override unreadable",
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
