package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/latebit-io/junto/engine/llm"
)

// defaultEvaluatorTimeout is the maximum duration for a single evaluator LLM call.
const defaultEvaluatorTimeout = 10 * time.Second

// maxEvaluatorRetries is the number of times the evaluator can reject an edit
// before letting it through. Prevents infinite self-correction loops.
const maxEvaluatorRetries = 2

// StyleEvaluator reviews proposed edits against coding style rules using
// a secondary LLM call. It catches design-level violations that static
// analysis cannot detect (responsibility splitting, abstraction quality,
// naming conventions, architectural layering).
type StyleEvaluator struct {
	provider llm.Provider
	rules    []string // formatted style rules from CodingStyleData
	timeout  time.Duration
}

// NewStyleEvaluator creates an evaluator. The provider can be a cheaper/faster
// model than the main agent (e.g. gemini-flash for classification).
func NewStyleEvaluator(provider llm.Provider, rules []string, timeout time.Duration) *StyleEvaluator {
	if timeout == 0 {
		timeout = defaultEvaluatorTimeout
	}
	return &StyleEvaluator{
		provider: provider,
		rules:    rules,
		timeout:  timeout,
	}
}

// Review sends the proposed edit to the evaluator model and returns
// violation descriptions. Returns nil when the edit passes review.
// Returns nil (not error) on timeout or provider failure — evaluator
// failures must not block the edit flow.
func (e *StyleEvaluator) Review(ctx context.Context, path, search, replace string) []string {
	if len(e.rules) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	prompt := buildEvaluatorPrompt(e.rules, path, search, replace)
	messages := []llm.Message{
		{Role: "system", Content: "You are a code style reviewer. Respond ONLY with a JSON array of violation strings. Empty array [] means the edit is compliant."},
		{Role: "user", Content: prompt},
	}

	ch, err := e.provider.Stream(ctx, messages, nil)
	if err != nil {
		slog.Warn("style evaluator: stream failed", "err", err)
		return nil
	}

	// Drain stream to collect the full response.
	var response strings.Builder
	for ev := range ch {
		if ev.Token != "" {
			response.WriteString(ev.Token)
		}
	}

	if ctx.Err() != nil {
		slog.Warn("style evaluator: timed out", "timeout", e.timeout)
		return nil
	}

	return parseViolations(response.String())
}

// buildEvaluatorPrompt constructs the review prompt for the evaluator.
func buildEvaluatorPrompt(rules []string, path, search, replace string) string {
	var b strings.Builder
	b.WriteString("Review this proposed edit against the coding style rules.\n\n")
	b.WriteString("## Rules\n\n")
	for _, r := range rules {
		b.WriteString("- ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	b.WriteString("\n## File: ")
	b.WriteString(path)
	b.WriteString("\n\n### Current code\n```\n")
	b.WriteString(search)
	b.WriteString("\n```\n\n### Proposed replacement\n```\n")
	b.WriteString(replace)
	b.WriteString("\n```\n\n")
	b.WriteString("If the edit violates any rules, respond with a JSON array of concise violation strings.\n")
	b.WriteString("If the edit is compliant, respond with: []\n")
	return b.String()
}

// parseViolations extracts a JSON string array from the evaluator response.
// Returns nil if the response is empty, not JSON, or an empty array.
// Tolerant of markdown code fences and surrounding whitespace.
func parseViolations(response string) []string {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil
	}

	// Strip markdown code fences if present.
	if strings.HasPrefix(response, "```") {
		lines := strings.Split(response, "\n")
		// Remove first and last lines (fences).
		if len(lines) >= 3 {
			response = strings.Join(lines[1:len(lines)-1], "\n")
			response = strings.TrimSpace(response)
		}
	}

	var violations []string
	if err := json.Unmarshal([]byte(response), &violations); err != nil {
		slog.Debug("style evaluator: cannot parse response as JSON array",
			"response", truncateLog(response), "err", err)
		return nil
	}

	if len(violations) == 0 {
		return nil
	}
	return violations
}

// truncateLog truncates a string for log output.
func truncateLog(s string) string {
	if len(s) > 200 {
		return s[:200] + fmt.Sprintf("... (%d bytes)", len(s))
	}
	return s
}
