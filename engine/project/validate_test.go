package project

import (
	"strings"
	"testing"
)

func TestValidate_Valid(t *testing.T) {
	src := `---
project: Test
---
# Phase 1: Setup
## Feature A
- [x] done task
- [ ] pending task
- [>] active task
# Phase 2: Cleanup
## Goal: remove legacy code
- [ ] drop old module
## Plan: rewrite types
- [ ] switch to v2 API
`
	errs := Validate(Parse(src))
	if errs != nil {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestValidate_EmptyTree(t *testing.T) {
	if errs := Validate(&Tree{}); errs != nil {
		t.Fatalf("expected no errors for empty tree (no roots), got: %v", errs)
	}
}

func TestValidate_NilTree(t *testing.T) {
	if errs := Validate(nil); errs != nil {
		t.Fatalf("expected nil for nil tree, got: %v", errs)
	}
}

func TestValidate_NoFrontmatterStillValid(t *testing.T) {
	// `project:` frontmatter is optional; projects commonly carry demarkus
	// metadata (version, hash) instead. Absence must not trigger an error.
	src := `# Phase 1: Setup
## Feature
- [ ] task
`
	if errs := Validate(Parse(src)); errs != nil {
		t.Errorf("expected no errors without project frontmatter, got: %v", errs)
	}
}

func TestValidate_InvalidPhaseHeading(t *testing.T) {
	cases := []string{
		`---
project: X
---
# Not a phase
## F
- [ ] t
`,
		`---
project: X
---
# Phase: missing number
## F
- [ ] t
`,
		`---
project: X
---
# Phase 1 missing colon
## F
- [ ] t
`,
	}
	for i, src := range cases {
		errs := Validate(Parse(src))
		if !containsCode(errs, ErrInvalidPhaseHeading) {
			t.Errorf("case %d: expected %s, got: %v", i, ErrInvalidPhaseHeading, errs)
		}
	}
}

func TestValidate_HeadingTooDeep(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## Feature
### Too Deep
- [ ] task
`
	errs := Validate(Parse(src))
	if !containsCode(errs, ErrHeadingTooDeep) {
		t.Errorf("expected %s, got: %v", ErrHeadingTooDeep, errs)
	}
}

func TestValidate_TaskOutsideFeature(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
- [ ] task directly under phase
`
	errs := Validate(Parse(src))
	if !containsCode(errs, ErrTaskOutsideFeature) {
		t.Errorf("expected %s, got: %v", ErrTaskOutsideFeature, errs)
	}
}

func TestValidate_MultipleActiveTasks(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [>] first active
- [>] second active
`
	errs := Validate(Parse(src))
	if !containsCode(errs, ErrMultipleActiveTasks) {
		t.Errorf("expected %s, got: %v", ErrMultipleActiveTasks, errs)
	}
}

func TestValidate_ZeroActiveTasksIsFine(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [ ] pending
- [x] done
`
	errs := Validate(Parse(src))
	if containsCode(errs, ErrMultipleActiveTasks) {
		t.Errorf("zero active tasks should be fine, got: %v", errs)
	}
}

func TestValidate_CollectsAllErrors(t *testing.T) {
	src := `# Not a phase
### Too deep
- [ ] orphan
# Phase 1: A
## F
- [>] a
- [>] b
`
	errs := Validate(Parse(src))
	want := []string{
		ErrInvalidPhaseHeading,
		ErrHeadingTooDeep,
		ErrMultipleActiveTasks,
	}
	for _, code := range want {
		if !containsCode(errs, code) {
			t.Errorf("expected %s in %v", code, errs)
		}
	}
}

func TestValidationErrors_Error(t *testing.T) {
	es := ValidationErrors{
		{Code: "a", Message: "first"},
		{Code: "b", Message: "second", Context: "x"},
	}
	got := es.Error()
	if !strings.Contains(got, "2 validation errors") ||
		!strings.Contains(got, "[a] first") ||
		!strings.Contains(got, "[b] second: \"x\"") {
		t.Errorf("unexpected formatting: %s", got)
	}

	single := ValidationErrors{{Code: "c", Message: "only"}}
	if single.Error() != "[c] only" {
		t.Errorf("single-error formatting wrong: %s", single.Error())
	}

	var empty ValidationErrors
	if empty.Error() != "" {
		t.Errorf("empty errors should format as empty string, got %q", empty.Error())
	}
}

func containsCode(errs ValidationErrors, code string) bool {
	for _, e := range errs {
		if e.Code == code {
			return true
		}
	}
	return false
}
