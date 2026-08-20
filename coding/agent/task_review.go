package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/lint"
	"github.com/latebit-io/nib/coding/smoke"
	enginelint "github.com/latebit-io/nib/engine/lint"
)

// siblingLintReportName is the file (under <projectRoot>/.project/) where
// non-blocking sibling-file lint findings are written so the count in the
// banner is inspectable rather than a dead-end number. Regenerated on
// every task completion that surfaces sibling findings.
const siblingLintReportName = "last-lint.txt"

// Post-task review pipeline for Agent.
//
// Fires when the LLM calls update_task(action:"complete"). The pipeline
// is two-stage and order-sensitive:
//
//   1. lint — runs the configured per-style linters against edited
//      files and aggregates findings. Edited-file findings are
//      blocking; sibling-file findings are reported but never block.
//   2. smoke run — invokes the resolved smoke command once at task
//      completion to verify the artifact actually launches.
//
// Each stage returns a string fragment appended to the tool result; the
// LLM sees the combined banner on its next turn.
//
// State remains on Agent (taskEdits, linters, smokeConfig, pendingLint,
// validatorRetries). Methods are grouped here so the review
// responsibility is visible at the file level rather than buried
// inside agent.go's run loop.

// runTaskReview runs lint and smoke on all files edited during the task.
// Called when update_task(action: "complete") fires. Returns the tool result
// with any lint/smoke feedback appended.
//
// Implements the three-state lint pipeline:
//
//  1. Infrastructure error — the linter itself failed (missing binary, bad
//     config, timeout). Surface to the developer via status banner; do NOT
//     inject as "violations found" into the LLM's next turn.
//  2. Clean — ran successfully, zero findings on edited files. Banner
//     reports sibling-file counts when present but never blocks.
//  3. Findings on edited files — inject structured findings into pendingLint
//     so the next turn fixes them. Craftsmanship policy: any finding on a
//     file the agent touched is blocking, pre-existing or not.
//
// Emits a status banner even when no checks are configured, so the developer
// can distinguish "task reviewed clean" from "nothing was set up to review."
func (a *Agent) runTaskReview(ctx context.Context, toolMsg string) string {
	a.mu.Lock()
	edits := a.taskEdits
	a.taskEdits = nil
	linters := slices.Clone(a.linters)
	a.mu.Unlock()

	var review strings.Builder
	review.WriteString(toolMsg)

	// Collect unique edited files and unique package directories. A task
	// that edits three files in one package lints one directory, not three.
	paths := make([]string, 0, len(edits))
	for _, e := range edits {
		paths = append(paths, e.Path)
	}
	editedFiles, editedDirs, filesByDir := lint.GroupPathsByDir(paths)

	lintWillRun := len(editedFiles) > 0 && len(linters) > 0
	smokeWillRun := len(editedFiles) > 0 && !a.smokeConfig.Skipped &&
		a.smokeConfig.Command != "" && os.Getenv(brand.EnvKeySmokeDisabled) == ""

	// Surface "no review configured" when we have work but nothing to check it
	// with. Silent return used to be indistinguishable from "clean"; now the
	// developer sees why.
	if len(editedFiles) > 0 && !lintWillRun && !smokeWillRun {
		a.send(event.AgentToken{Text: "\n[Task complete — no lint or smoke run configured]\n"})
	}

	if lintWillRun {
		a.runLinters(ctx, linters, editedFiles, editedDirs, filesByDir)
	}

	// Smoke runs verify the artifact actually launches — runtime errors
	// the parser/lint/architecture stages cannot catch. The result is
	// appended to the review so the LLM sees it on the next turn (it
	// can decide to reopen the task and fix the regression).
	if smokeWillRun {
		smokeMsg := a.runSmokeReview(ctx)
		if smokeMsg != "" {
			review.WriteString("\n\n")
			review.WriteString(smokeMsg)
		}
	}

	return review.String()
}

// runLinters executes each configured linter once per edited directory and
// emits the three-state banner + injects findings into pendingLint when
// any finding lands on an edited file.
func (a *Agent) runLinters(ctx context.Context, linters []enginelint.Linter, editedFiles, editedDirs []string, filesByDir map[string][]string) {
	a.send(event.AgentStatus{Status: event.StatusLinting})
	a.send(event.AgentToken{Text: "\n[Task complete — running style lint...]\n"})

	editedSet := make(map[string]bool, len(editedFiles))
	for _, f := range editedFiles {
		editedSet[f] = true
	}

	var (
		editedFindings  []enginelint.Finding
		siblingFindings []enginelint.Finding
		infraErrors     []infraError
		projectRoot     = a.workspace.ProjectRoot()
	)

	for _, dir := range editedDirs {
		for _, l := range linters {
			res := l.Run(ctx, projectRoot, dir, filesByDir[dir])
			if res.Error != nil {
				slog.Warn("lint: adapter failed", "linter", l.Name(), "dir", dir, "err", res.Error)
				infraErrors = append(infraErrors, infraError{name: l.Name(), err: res.Error})
				continue
			}
			for _, f := range res.Findings {
				if editedSet[f.Path] {
					editedFindings = append(editedFindings, f)
				} else {
					// Retain — not just count. The findings are persisted
					// below so the banner's count is inspectable instead of
					// a dead-end number the agent cannot expand.
					siblingFindings = append(siblingFindings, f)
				}
			}
		}
	}

	for _, ie := range infraErrors {
		a.send(event.AgentToken{Text: fmt.Sprintf("[Style lint: %s failed — %v — not blocking]\n", ie.name, ie.err)})
	}

	siblingNote := a.siblingLintNote(projectRoot, siblingFindings)

	if len(editedFindings) > 0 {
		a.send(event.AgentToken{Text: "[Style lint: violations found — fix before next task]\n"})
		if siblingNote != "" {
			a.send(event.AgentToken{Text: "[Style lint: " + siblingNote + "]\n"})
		}
		a.mu.Lock()
		a.pendingLint = lint.FormatFindings(editedFindings)
		a.mu.Unlock()
		return
	}

	banner := "[Style lint: clean ✓]"
	if siblingNote != "" {
		banner = "[Style lint: clean ✓ (" + siblingNote + ")]"
	}
	a.send(event.AgentToken{Text: banner + "\n"})
}

// siblingLintNote builds the non-blocking sibling-file note for the lint
// banner. It persists the findings to an inspectable report so the count
// is actionable (the agent can read_file it; the developer can open it)
// and references the report path. On write failure — or when there is no
// project root to anchor the file — it degrades to the bare count rather
// than blocking or losing the signal. Returns "" when there are no
// sibling findings.
func (a *Agent) siblingLintNote(projectRoot string, findings []enginelint.Finding) string {
	if len(findings) == 0 {
		return ""
	}
	note := fmt.Sprintf("%d pre-existing in sibling files — not blocking", len(findings))
	path, err := a.writeSiblingLintReport(projectRoot, findings)
	if err != nil {
		slog.Warn("lint: write sibling report failed", "err", err)
		return note
	}
	if path == "" {
		return note
	}
	return note + "; details: " + path
}

// writeSiblingLintReport persists the non-blocking sibling-file findings
// to <projectRoot>/.project/[siblingLintReportName] and returns the
// project-relative path for display. Returns ("", nil) when the workspace
// has no project root (nothing to anchor the file to) so the caller falls
// back to a count-only note. The report is overwritten each task
// completion — it always reflects the latest review.
func (a *Agent) writeSiblingLintReport(projectRoot string, findings []enginelint.Finding) (string, error) {
	if projectRoot == "" {
		return "", nil
	}
	dir := filepath.Join(projectRoot, ".project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create .project dir: %w", err)
	}

	var b strings.Builder
	b.WriteString("Style lint — pre-existing findings in sibling files (not blocking)\n")
	fmt.Fprintf(&b, "%d finding(s); regenerated on each task completion.\n\n", len(findings))
	b.WriteString(lint.FormatFindings(findings))
	b.WriteByte('\n')

	if err := os.WriteFile(filepath.Join(dir, siblingLintReportName), []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write lint report: %w", err)
	}
	return filepath.Join(".project", siblingLintReportName), nil
}

// infraError pairs an adapter name with the infrastructure error it raised,
// so the banner text can reference the linter even after aggregation.
type infraError struct {
	name string
	err  error
}

// runSmokeReview invokes the configured smoke-run command once at task
// completion and returns a formatted result for inclusion in the
// runTaskReview output. Returns an empty string only when the
// configuration is Skipped at call time (the [brand.EnvKeySmokeDisabled]
// env var path is checked by the caller).
//
// Smoke runs do not consume the per-path validatorRetries budget — that
// counter is per-edit, while smoke fires per task. The LLM's incentive
// to fix runtime regressions comes from the failure being included in
// the update_task tool result; under autonomous mode it will iterate
// without further plumbing, and under guided mode the developer sees
// the failure and decides whether to push the agent forward.
func (a *Agent) runSmokeReview(ctx context.Context) string {
	cfg := a.smokeConfig
	if cfg.Skipped {
		return ""
	}
	// Flip the phase before the blocking RunSmoke call below. Without this
	// the frontend status holds the prior StatusLinting for the entire
	// smoke run — which launches the artifact and can block for the
	// lifetime of that process.
	a.send(event.AgentStatus{Status: event.StatusSmoke})
	// Surface only the source (make-smoke / lua-main / config / …)
	// not the resolved command — `.project/run.json` may contain
	// inline env assignments or auth flags, and this banner ends up
	// in the capture sink (and from there in the session journal,
	// which can be distributed). The command itself reaches debug
	// logs (process-local) and the actual exec, both of which are
	// dev-machine-local; persisted surfaces stay redacted.
	a.send(event.AgentToken{Text: fmt.Sprintf("\n[Smoke run: %s]\n", cfg.Source)})
	res := smoke.RunSmoke(ctx, a.workspace.ProjectRoot(), cfg)
	return smoke.FormatSmokeResult(cfg, res)
}
