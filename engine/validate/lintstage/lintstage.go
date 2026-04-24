// Package lintstage adapts the engine/lint per-file linters into a
// pre-approval [validate.Validator], so per-edit feedback (luacheck,
// gofmt-style checks) reaches the LLM through the same Retry budget
// the parser and architecture stages already use.
//
// Only per-file safe linters run here — adapters that depend on a
// surrounding package layout (golangci-lint, go vet) stay on the
// post-task review path because the validator writes the candidate's
// After content to a temp file in isolation.
package lintstage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/junto/engine/lint"
	"github.com/latebit-io/junto/engine/validate"
)

// StageName identifies this validator in [validate.Result.Stage] and
// capture payloads.
const StageName = "lint"

// maxReportedFindings caps how many findings appear in a single retry
// feedback string so a noisy linter cannot blow out the model context.
const maxReportedFindings = 8

// LinterSource returns the per-file linters available for the current
// project. The validator calls it once per Validate, so style cycles
// or runtime config changes that swap the linter set are picked up
// without rebuilding the pipeline.
type LinterSource func() []lint.Linter

// Validator is the per-file lint pre-approval validator.
type Validator struct {
	source LinterSource
}

// New constructs a Validator wired to the given linter source. A nil
// source — or one that returns nil — makes the validator a no-op:
// Applicable returns false, Validate returns Pass.
func New(source LinterSource) *Validator {
	return &Validator{source: source}
}

// Name returns the stable stage identifier.
func (Validator) Name() string { return StageName }

// Applicable reports whether the validator should run for c. Returns
// false when no per-file linters are registered or when the candidate
// has no extension — extensionless files are typically scripts or
// binaries that lint adapters cannot safely process.
func (v *Validator) Applicable(c validate.Candidate) bool {
	if v.source == nil {
		return false
	}
	if filepath.Ext(c.Path) == "" {
		return false
	}
	return len(v.source()) > 0
}

// Validate writes c.After to a temp file with the original extension
// preserved, runs each per-file linter against it, aggregates findings,
// and rewrites their Path fields back to c.Path so the LLM sees the
// real source location instead of /tmp/junto-lint-…/main.lua.
//
// Verdict is Retry on findings, Pass on clean. An infrastructure error
// from a linter (missing binary, parse failure) does not produce Retry
// — it is logged and treated as Pass so a misconfigured linter cannot
// block the developer's flow. Post-task review surfaces such errors via
// its banner.
func (v *Validator) Validate(ctx context.Context, c validate.Candidate) validate.Result {
	if v.source == nil {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}
	linters := v.source()
	if len(linters) == 0 {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	tempDir, tempFile, cleanup, err := writeCandidate(c.Path, c.After)
	if err != nil {
		slog.Warn("lintstage: cannot stage temp file", "err", err)
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}
	defer cleanup()

	var findings []lint.Finding
	for _, l := range linters {
		if err := ctx.Err(); err != nil {
			break
		}
		res := l.Run(ctx, tempDir, ".", []string{tempFile})
		if res.Error != nil {
			slog.Warn("lintstage: linter run failed", "linter", l.Name(), "err", res.Error)
			continue
		}
		findings = append(findings, res.Findings...)
	}

	if len(findings) == 0 {
		return validate.Result{Verdict: validate.Pass, Stage: StageName}
	}

	rewritePaths(findings, c.Path)

	return validate.Result{
		Verdict:  validate.Retry,
		Stage:    StageName,
		Findings: findings,
		Feedback: formatFeedback(c.Path, findings),
	}
}

// writeCandidate creates a sandbox temp directory and writes content to
// a file inside it that mirrors the original extension. Returns the
// directory, the file basename (relative to dir, ready for Linter.Run's
// files argument), and a cleanup callback that removes the directory.
//
// Cleanup is best-effort — failures are logged via slog but do not
// surface to callers because a leaked tempfile is preferable to a
// confusing error tail on a successful validation.
func writeCandidate(originalPath, content string) (dir, file string, cleanup func(), err error) {
	tempDir, err := os.MkdirTemp("", "junto-lint-")
	if err != nil {
		return "", "", nil, fmt.Errorf("mkdir temp: %w", err)
	}

	base := filepath.Base(originalPath)
	if base == "" || base == "." || base == string(os.PathSeparator) {
		base = "candidate" + filepath.Ext(originalPath)
	}
	target := filepath.Join(tempDir, base)
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", "", nil, fmt.Errorf("write temp: %w", err)
	}

	cleanup = func() {
		if err := os.RemoveAll(tempDir); err != nil {
			slog.Warn("lintstage: cleanup failed", "dir", tempDir, "err", err)
		}
	}
	return tempDir, base, cleanup, nil
}

// rewritePaths replaces the linter-reported Path on every finding with
// the candidate's real path. Per-file lint runs are scoped to a single
// staged file in an isolated sandbox dir, so any finding the linter
// emits is necessarily about that file. Rewriting unconditionally
// avoids fragile basename matching when a linter reports either bare
// names or absolute sandbox paths.
func rewritePaths(findings []lint.Finding, realPath string) {
	for i := range findings {
		findings[i].Path = realPath
	}
}

// formatFeedback renders findings into a retry prompt the LLM can act
// on. Caps the rendered list at maxReportedFindings so a noisy linter
// does not blow out the model's context window — the count of elided
// findings is preserved so the LLM knows there are more.
func formatFeedback(path string, findings []lint.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Per-file lint flagged %d issue(s) in %s. Fix them and propose the edit again.\n\n",
		len(findings), path)

	limit := min(maxReportedFindings, len(findings))
	for _, f := range findings[:limit] {
		linterTag := f.Linter
		if linterTag == "" {
			linterTag = StageName
		}
		switch {
		case f.Line > 0 && f.Col > 0:
			fmt.Fprintf(&b, "  - line %d:%d — %s (%s)\n", f.Line, f.Col, f.Message, linterTag)
		case f.Line > 0:
			fmt.Fprintf(&b, "  - line %d — %s (%s)\n", f.Line, f.Message, linterTag)
		default:
			fmt.Fprintf(&b, "  - %s (%s)\n", f.Message, linterTag)
		}
	}
	if len(findings) > limit {
		fmt.Fprintf(&b, "  … and %d more\n", len(findings)-limit)
	}
	return b.String()
}
