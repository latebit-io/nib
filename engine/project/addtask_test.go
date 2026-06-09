package project

import (
	"strings"
	"testing"
)

func TestAddTask_AppendsUnderExistingFeature(t *testing.T) {
	src := `---
project: X
---
# Phase 1: Setup
## Feature A
- [x] done
`
	tree := Parse(src)
	if err := tree.AddTask("Phase 1", "Feature A", "new task", ""); err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	body := Serialize(tree)
	if !strings.Contains(body, "- [ ] new task") {
		t.Errorf("expected new task in body, got:\n%s", body)
	}
	// Validate still passes.
	if errs := Validate(Parse(body)); errs != nil {
		t.Errorf("validate after add failed: %v", errs)
	}
}

func TestAddTask_CreatesFeatureIfMissing(t *testing.T) {
	src := `---
project: X
---
# Phase 1: Setup
## Existing
- [x] done
`
	tree := Parse(src)
	if err := tree.AddTask("Phase 1", "Brand New Feature", "task", ""); err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	body := Serialize(tree)
	if !strings.Contains(body, "## Brand New Feature") {
		t.Errorf("expected new feature heading, got:\n%s", body)
	}
	if !strings.Contains(body, "- [ ] task") {
		t.Errorf("expected task, got:\n%s", body)
	}
}

func TestAddTask_WithSupplementaryLink(t *testing.T) {
	src := `---
project: X
---
# Phase 1: Game Engine
## Rendering
`
	tree := Parse(src)
	err := tree.AddTask("Phase 1", "Rendering", "Build tile map", "/game/tilemap.md")
	if err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	body := Serialize(tree)
	want := "- [ ] Build tile map ([details](/game/tilemap.md))"
	if !strings.Contains(body, want) {
		t.Errorf("expected %q in body, got:\n%s", want, body)
	}
}

func TestAddTask_PhaseMatchesBySubstring(t *testing.T) {
	src := `---
project: X
---
# Phase 1: Persistence
## A
# Phase 2: Rendering
## B
`
	tree := Parse(src)
	// Substring "Render" should find Phase 2.
	if err := tree.AddTask("Render", "B", "task under B", ""); err != nil {
		t.Fatalf("AddTask failed: %v", err)
	}
	body := Serialize(tree)
	parts := strings.SplitN(body, "# Phase 2", 2)
	if len(parts) != 2 {
		t.Fatalf("expected Phase 2 heading in output:\n%s", body)
	}
	phase1Section, phase2Section := parts[0], parts[1]
	// Positive: the task must appear under Phase 2.
	if !strings.Contains(phase2Section, "task under B") {
		t.Errorf("task did not land under Phase 2:\n%s", body)
	}
	// Negative: and must not appear under Phase 1.
	if strings.Contains(phase1Section, "task under B") {
		t.Errorf("task leaked into Phase 1:\n%s", body)
	}
}

func TestAddTask_UnknownPhase(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
`
	tree := Parse(src)
	err := tree.AddTask("Phase 99", "F", "task", "")
	if err == nil {
		t.Fatalf("expected error for unknown phase")
	}
	if !strings.Contains(err.Error(), "no phase matching") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestAddTask_RejectsTreeWideDuplicate locks the dedup contract: a
// task body that already exists anywhere in the tree is refused, even
// when the new entry targets a different (phase, feature). The
// activate/complete tools resolve by title across the whole tree, so
// two same-bodied tasks would make them ambiguous.
func TestAddTask_RejectsTreeWideDuplicate(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F1
- [ ] Implement Cruise Elroy behavior for Blinky
# Phase 2: B
## F2
`
	tree := Parse(src)
	// Identical body under a different (phase, feature) — must fail.
	err := tree.AddTask("Phase 2", "F2", "Implement Cruise Elroy behavior for Blinky", "")
	if err == nil {
		t.Fatalf("expected duplicate rejection")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("unexpected error: %v", err)
	}
	// And the tree must not have the duplicate.
	body := Serialize(tree)
	if strings.Count(body, "Implement Cruise Elroy") != 1 {
		t.Errorf("expected exactly one Cruise Elroy task in tree:\n%s", body)
	}
}

func TestAddTask_DuplicateDetectionCaseInsensitive(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [ ] Build Tile Map
`
	tree := Parse(src)
	err := tree.AddTask("Phase 1", "F", "build tile map", "")
	if err == nil {
		t.Fatalf("expected duplicate rejection for case-different body")
	}
}

// TestAddTask_DuplicateDetectionIgnoresLinkSuffix covers the
// "added once with no link, then re-added with a link" mistake — the
// stored title carries the " ([details](...))" suffix but the body
// equality check must collapse the two.
func TestAddTask_DuplicateDetectionIgnoresLinkSuffix(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [ ] Build tile map ([details](/game/tilemap.md))
`
	tree := Parse(src)
	err := tree.AddTask("Phase 1", "F", "Build tile map", "")
	if err == nil {
		t.Fatalf("expected duplicate rejection ignoring link suffix")
	}
}

// TestAddTask_DuplicateDetectionTrimsStoredTitle covers manually-edited
// project.md content where a title carries surrounding whitespace. The
// AddTask writer trims before insertion, but a hand-authored markdown
// task can land with trailing spaces; without symmetric trim on the
// stored-title side the dedup would silently miss the duplicate.
func TestAddTask_DuplicateDetectionTrimsStoredTitle(t *testing.T) {
	// Synthesize a tree with a trailing-whitespace task title that the
	// parser would not normalize. Going through Parse is the realistic
	// path; if a future parser trims this we can drop the test, but the
	// guard belongs at the dedup site regardless.
	tree := &Tree{
		Roots: []*Node{
			{
				Title:     "Phase 1: A",
				Depth:     0,
				IsHeading: true,
				Children: []*Node{
					{
						Title:     "F",
						Depth:     1,
						IsHeading: true,
						Children: []*Node{
							{
								Title:     "  spaced task  ",
								Depth:     2,
								IsHeading: false,
								Status:    TaskPending,
							},
						},
					},
				},
			},
		},
	}
	err := tree.AddTask("Phase 1", "F", "spaced task", "")
	if err == nil {
		t.Fatalf("expected duplicate rejection when stored title has surrounding whitespace")
	}
}

// TestAddTask_DistinctTaskAllowed sanity-checks that the dedup doesn't
// over-reject — a clearly different body still lands.
func TestAddTask_DistinctTaskAllowed(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [ ] Build tile map
`
	tree := Parse(src)
	if err := tree.AddTask("Phase 1", "F", "Render sprite atlas", ""); err != nil {
		t.Fatalf("unrelated task should land: %v", err)
	}
	if !strings.Contains(Serialize(tree), "- [ ] Render sprite atlas") {
		t.Errorf("expected new task in serialized tree")
	}
}

// TestStripDetailsSuffix exercises the suffix-strip helper directly so
// regressions on the format don't slip through via the higher-level
// dedup tests.
func TestStripDetailsSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain title", "plain title"},
		{"Build tile map ([details](/game/tilemap.md))", "Build tile map"},
		{"Nested parens (x) ([details](/p.md))", "Nested parens (x)"},
		{"trailing parens))", "trailing parens))"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripDetailsSuffix(tc.in); got != tc.want {
			t.Errorf("stripDetailsSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAddTask_RoundTripsThroughSerializer(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [x] old
`
	tree := Parse(src)
	if err := tree.AddTask("Phase 1", "F", "fresh", "/notes/x.md"); err != nil {
		t.Fatalf("first AddTask failed: %v", err)
	}
	if err := tree.AddTask("Phase 1", "F2", "second", ""); err != nil {
		t.Fatalf("second AddTask failed: %v", err)
	}

	body := Serialize(tree)
	tree2 := Parse(body)
	if errs := Validate(tree2); errs != nil {
		t.Fatalf("round-trip validation failed: %v\n%s", errs, body)
	}
	// Re-serialize and confirm stable.
	body2 := Serialize(tree2)
	if body != body2 {
		t.Errorf("round trip not stable:\n--- first ---\n%s\n--- second ---\n%s", body, body2)
	}
}
