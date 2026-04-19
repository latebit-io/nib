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
	// Tree already has B under phase 2; verify task landed there.
	body := Serialize(tree)
	// Phase 1 A must not have the task.
	if strings.Contains(strings.Split(body, "# Phase 2")[0], "task under B") {
		t.Errorf("task landed under wrong phase:\n%s", body)
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

func TestAddTask_RoundTripsThroughSerializer(t *testing.T) {
	src := `---
project: X
---
# Phase 1: A
## F
- [x] old
`
	tree := Parse(src)
	_ = tree.AddTask("Phase 1", "F", "fresh", "/notes/x.md")
	_ = tree.AddTask("Phase 1", "F2", "second", "")

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
