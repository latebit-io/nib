package agent

import (
	"github.com/latebit-io/nib/coding/prompts"
)

// CodingStyleData aliases [prompts.CodingStyleData] so wire/tui
// callers keep their existing identifier while the canonical type
// lives in `coding/prompts`.
type CodingStyleData = prompts.CodingStyleData

// StyleRule aliases [prompts.StyleRule].
type StyleRule = prompts.StyleRule

// SystemPromptData aliases [prompts.SystemPromptData].
type SystemPromptData = prompts.SystemPromptData

// UserPromptData aliases [prompts.UserPromptData].
type UserPromptData = prompts.UserPromptData

// PromptLoader aliases [prompts.PromptLoader].
type PromptLoader = prompts.PromptLoader

// NewCodingStyleData constructs a [CodingStyleData].
func NewCodingStyleData(name string, rules []StyleRule) *CodingStyleData {
	return prompts.NewCodingStyleData(name, rules)
}

// NewPromptLoader constructs a [PromptLoader] rooted at projectRoot.
func NewPromptLoader(projectRoot string) *PromptLoader {
	return prompts.NewPromptLoader(projectRoot)
}

// DetectLanguage returns a human-readable language name inferred from
// path's extension. Delegates to [prompts.DetectLanguage].
func DetectLanguage(path string) string {
	return prompts.DetectLanguage(path)
}
