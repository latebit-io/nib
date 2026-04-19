package project

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidationError describes a single schema violation found in a work tree.
type ValidationError struct {
	// Code is a short machine-readable tag identifying the violation kind.
	Code string
	// Message is a human-readable description of the violation.
	Message string
	// Context is the offending heading or task title, if applicable.
	// Empty when the violation has no specific anchor (e.g. missing frontmatter).
	Context string
}

// Error formats the violation as "[code] message (context)".
func (e ValidationError) Error() string {
	if e.Context == "" {
		return fmt.Sprintf("[%s] %s", e.Code, e.Message)
	}
	return fmt.Sprintf("[%s] %s: %q", e.Code, e.Message, e.Context)
}

// ValidationErrors aggregates every violation found in a single pass.
// Implements error so callers can use idiomatic error handling while still
// accessing the individual violations for structured reporting.
type ValidationErrors []ValidationError

// Error formats all violations as a newline-separated list.
func (es ValidationErrors) Error() string {
	switch len(es) {
	case 0:
		return ""
	case 1:
		return es[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation errors:", len(es))
	for _, e := range es {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

// Validation error codes. Each is stable and safe to pattern-match on.
const (
	// ErrInvalidPhaseHeading indicates an h1 that does not match `Phase N: Title`.
	ErrInvalidPhaseHeading = "invalid-h1-format"
	// ErrHeadingTooDeep indicates an h3 or deeper heading, which is not allowed.
	ErrHeadingTooDeep = "h3-not-allowed"
	// ErrTaskOutsideFeature indicates a task not placed under an h2.
	ErrTaskOutsideFeature = "task-outside-feature"
	// ErrMultipleActiveTasks indicates more than one `- [>]` task in the doc.
	ErrMultipleActiveTasks = "multiple-active-tasks"
	// ErrEmptyTitle indicates a heading or task with no title content.
	ErrEmptyTitle = "empty-title"
	// ErrRootNotPhase indicates a root-level node that isn't a phase heading.
	ErrRootNotPhase = "root-not-phase"
)

// phaseHeadingPattern matches "Phase N: Title" where N is one or more digits
// and Title is any non-empty text. Trailing whitespace is tolerated.
var phaseHeadingPattern = regexp.MustCompile(`^Phase\s+\d+:\s+\S.*$`)

// Validate checks a parsed work tree against the strict project.md schema.
// Returns nil when clean, otherwise a ValidationErrors slice containing every
// violation found in a single pass (callers see all problems at once).
//
// Rules enforced:
//   - Every root node must be a heading matching `Phase N: Title`.
//   - No heading deeper than h2 (h3/h4/h5/h6 are rejected).
//   - Tasks must live directly under an h2 (depth 2 nodes with a heading parent).
//   - At most one task may carry TaskActive status.
//   - Headings and tasks must have non-empty titles.
//
// The `project:` frontmatter field is optional — projects commonly carry
// demarkus metadata (version, hash) instead, and the display name is derived
// elsewhere. If `project:` is present it is not further constrained here.
func Validate(tree *Tree) ValidationErrors {
	var errs ValidationErrors

	if tree == nil {
		return nil
	}

	activeCount := 0
	for _, root := range tree.Roots {
		if !root.IsHeading {
			errs = append(errs, ValidationError{
				Code:    ErrRootNotPhase,
				Message: "root-level tasks are not allowed; tasks must live under a phase → feature",
				Context: root.Title,
			})
			continue
		}
		if root.Depth != 0 {
			// Parser should never emit this, but guard anyway.
			errs = append(errs, ValidationError{
				Code:    ErrRootNotPhase,
				Message: "root heading has non-zero depth",
				Context: root.Title,
			})
			continue
		}
		if !phaseHeadingPattern.MatchString(root.Title) {
			errs = append(errs, ValidationError{
				Code:    ErrInvalidPhaseHeading,
				Message: "h1 must match `Phase N: Title`",
				Context: root.Title,
			})
		}
		walkValidate(root, &errs, &activeCount)
	}

	if activeCount > 1 {
		errs = append(errs, ValidationError{
			Code:    ErrMultipleActiveTasks,
			Message: fmt.Sprintf("exactly one active task (`- [>]`) is allowed per document; found %d", activeCount),
		})
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// walkValidate recurses through a heading subtree, collecting violations.
// activeCount is incremented for every task with TaskActive status.
func walkValidate(n *Node, errs *ValidationErrors, activeCount *int) {
	for _, child := range n.Children {
		if child.IsHeading {
			if child.Depth > 1 {
				*errs = append(*errs, ValidationError{
					Code:    ErrHeadingTooDeep,
					Message: "headings deeper than h2 are not allowed",
					Context: child.Title,
				})
			}
			if strings.TrimSpace(child.Title) == "" {
				*errs = append(*errs, ValidationError{
					Code:    ErrEmptyTitle,
					Message: "heading has empty title",
				})
			}
			walkValidate(child, errs, activeCount)
			continue
		}

		// Task node.
		if strings.TrimSpace(child.Title) == "" {
			*errs = append(*errs, ValidationError{
				Code:    ErrEmptyTitle,
				Message: "task has empty title",
			})
		}
		if child.Depth != 2 {
			*errs = append(*errs, ValidationError{
				Code:    ErrTaskOutsideFeature,
				Message: "tasks must live directly under an h2 feature heading",
				Context: child.Title,
			})
		}
		if child.Status == TaskActive {
			*activeCount++
		}
	}
}
