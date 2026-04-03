package project

import (
	"testing"
)

func TestSerialize_Roundtrip(t *testing.T) {
	input := `---
project: Junto
---

# TUI Editor + Agent
## Phase 6: Plans
### Planning Mode
- [>] Add Phase to Session
- [ ] Tool filtering
### Breakdown View
- [ ] Parse project.md
## Phase 7: Plugin System
`
	tree := Parse(input)
	output := Serialize(tree)
	reparsed := Parse(output)

	// Verify structural equivalence after round-trip.
	if reparsed.ProjectName != tree.ProjectName {
		t.Errorf("ProjectName = %q, want %q", reparsed.ProjectName, tree.ProjectName)
	}
	assertTreesEqual(t, tree.Roots, reparsed.Roots, "")
}

func TestSerialize_NoFrontmatter(t *testing.T) {
	tree := &Tree{
		Roots: []*Node{
			{Title: "Component", Depth: 0, IsHeading: true},
		},
	}
	output := Serialize(tree)

	want := "# Component\n"
	if output != want {
		t.Errorf("output = %q, want %q", output, want)
	}
}

func TestSerialize_WithFrontmatter(t *testing.T) {
	tree := &Tree{
		ProjectName: "Junto",
		Roots: []*Node{
			{Title: "Component", Depth: 0, IsHeading: true},
		},
	}
	output := Serialize(tree)

	want := "---\nproject: Junto\n---\n\n# Component\n"
	if output != want {
		t.Errorf("output = %q, want %q", output, want)
	}
}

func TestSerialize_TaskStatuses(t *testing.T) {
	tree := &Tree{
		Roots: []*Node{
			{
				Title: "Feature", Depth: 0, IsHeading: true,
				Children: []*Node{
					{Title: "pending", Depth: 1, Status: TaskPending},
					{Title: "done", Depth: 1, Status: TaskDone},
					{Title: "active", Depth: 1, Status: TaskActive},
				},
			},
		},
	}
	output := Serialize(tree)

	want := "# Feature\n- [ ] pending\n- [x] done\n- [>] active\n"
	if output != want {
		t.Errorf("output = %q, want %q", output, want)
	}
}

func TestSerialize_EmptyTree(t *testing.T) {
	tree := &Tree{}
	output := Serialize(tree)
	if output != "" {
		t.Errorf("output = %q, want empty", output)
	}
}

func TestSerialize_NilTree(t *testing.T) {
	output := Serialize(nil)
	if output != "" {
		t.Errorf("output = %q, want empty", output)
	}
}

func TestSerialize_DeepNesting(t *testing.T) {
	tree := &Tree{
		Roots: []*Node{
			{
				Title: "L1", Depth: 0, IsHeading: true,
				Children: []*Node{
					{
						Title: "L2", Depth: 1, IsHeading: true,
						Children: []*Node{
							{
								Title: "L3", Depth: 2, IsHeading: true,
								Children: []*Node{
									{Title: "task", Depth: 3, Status: TaskPending},
								},
							},
						},
					},
				},
			},
		},
	}
	output := Serialize(tree)
	want := "# L1\n## L2\n### L3\n- [ ] task\n"
	if output != want {
		t.Errorf("output = %q, want %q", output, want)
	}
}

func TestSerialize_MultipleRoots(t *testing.T) {
	tree := &Tree{
		Roots: []*Node{
			{Title: "A", Depth: 0, IsHeading: true},
			{Title: "B", Depth: 0, IsHeading: true},
		},
	}
	output := Serialize(tree)
	want := "# A\n\n# B\n"
	if output != want {
		t.Errorf("output = %q, want %q", output, want)
	}
}

// assertTreesEqual compares two node slices recursively.
func assertTreesEqual(t *testing.T, a, b []*Node, path string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: len = %d vs %d", path, len(a), len(b))
	}
	for i := range a {
		p := path + "/" + a[i].Title
		if a[i].Title != b[i].Title {
			t.Errorf("%s: Title = %q vs %q", p, a[i].Title, b[i].Title)
		}
		if a[i].Depth != b[i].Depth {
			t.Errorf("%s: Depth = %d vs %d", p, a[i].Depth, b[i].Depth)
		}
		if a[i].IsHeading != b[i].IsHeading {
			t.Errorf("%s: IsHeading = %v vs %v", p, a[i].IsHeading, b[i].IsHeading)
		}
		if a[i].Status != b[i].Status {
			t.Errorf("%s: Status = %v vs %v", p, a[i].Status, b[i].Status)
		}
		assertTreesEqual(t, a[i].Children, b[i].Children, p)
	}
}
