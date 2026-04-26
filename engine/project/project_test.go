package project

import (
	"testing"
)

func TestTree_ActiveGoal(t *testing.T) {
	input := `# Component
## Phase
### Feature
- [ ] pending
- [>] active goal
- [ ] another
`
	tree := Parse(input)
	goal, ancestry := tree.ActiveGoal()

	if goal == nil {
		t.Fatal("ActiveGoal returned nil")
	}
	if goal.Title != "active goal" {
		t.Errorf("goal.Title = %q, want %q", goal.Title, "active goal")
	}
	if len(ancestry) != 4 {
		t.Fatalf("ancestry len = %d, want 4", len(ancestry))
	}

	wantPath := []string{"Component", "Phase", "Feature", "active goal"}
	for i, n := range ancestry {
		if n.Title != wantPath[i] {
			t.Errorf("ancestry[%d] = %q, want %q", i, n.Title, wantPath[i])
		}
	}
}

// TestTree_FindNextPendingTask verifies document-order traversal
// returns the first leaf task with TaskPending. Skips active and
// done tasks, walks across phases and features. Returns "" when no
// pending task remains so the agent's auto-continue path knows to
// stop.
func TestTree_FindNextPendingTask(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		md   string
		want string
	}{
		{
			name: "skips active and done, returns next pending",
			md: `# Phase
## Feature
- [x] done
- [>] active
- [ ] first pending
- [ ] second pending
`,
			want: "first pending",
		},
		{
			name: "no pending returns empty",
			md: `# Phase
## Feature
- [x] done
- [>] active
`,
			want: "",
		},
		{
			name: "empty tree returns empty",
			md:   ``,
			want: "",
		},
		{
			name: "walks across features",
			md: `# Phase
## Feature A
- [x] done a1
## Feature B
- [ ] pending b1
`,
			want: "pending b1",
		},
		{
			name: "walks across phases",
			md: `# Phase 1
## F
- [x] done
# Phase 2
## F
- [ ] target
`,
			want: "target",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := Parse(tc.md)
			if got := tree.FindNextPendingTask(); got != tc.want {
				t.Errorf("FindNextPendingTask() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTree_ActiveGoal_None(t *testing.T) {
	input := "# A\n- [ ] pending\n"
	tree := Parse(input)
	goal, ancestry := tree.ActiveGoal()

	if goal != nil {
		t.Errorf("goal = %v, want nil", goal)
	}
	if ancestry != nil {
		t.Errorf("ancestry = %v, want nil", ancestry)
	}
}

func TestTree_SetActiveGoal(t *testing.T) {
	input := `# A
- [>] old active
- [ ] new target
`
	tree := Parse(input)

	ok := tree.SetActiveGoal("new target")
	if !ok {
		t.Fatal("SetActiveGoal returned false")
	}

	// Old active should be cleared.
	if tree.Roots[0].Children[0].Status != TaskPending {
		t.Errorf("old active status = %v, want TaskPending", tree.Roots[0].Children[0].Status)
	}
	// New target should be active.
	if tree.Roots[0].Children[1].Status != TaskActive {
		t.Errorf("new target status = %v, want TaskActive", tree.Roots[0].Children[1].Status)
	}
}

func TestTree_SetActiveGoal_NotFound(t *testing.T) {
	tree := Parse("# A\n- [ ] task\n")
	ok := tree.SetActiveGoal("nonexistent")
	if ok {
		t.Error("SetActiveGoal returned true for nonexistent title")
	}
}

func TestTree_SetActiveGoal_RejectsHeading(t *testing.T) {
	tree := Parse("# Heading\n- [ ] task\n")
	ok := tree.SetActiveGoal("Heading")
	if ok {
		t.Error("SetActiveGoal returned true for heading node")
	}
}

func TestTree_MarkDone(t *testing.T) {
	input := "# A\n- [>] active task\n- [ ] pending task\n"
	tree := Parse(input)

	ok := tree.MarkDone("active task")
	if !ok {
		t.Fatal("MarkDone returned false")
	}
	if tree.Roots[0].Children[0].Status != TaskDone {
		t.Errorf("status = %v, want TaskDone", tree.Roots[0].Children[0].Status)
	}
}

func TestTree_MarkDone_NotFound(t *testing.T) {
	tree := Parse("# A\n- [ ] task\n")
	if tree.MarkDone("nonexistent") {
		t.Error("MarkDone returned true for nonexistent task")
	}
}

func TestTree_MarkDone_RejectsHeading(t *testing.T) {
	tree := Parse("# Heading\n")
	if tree.MarkDone("Heading") {
		t.Error("MarkDone returned true for heading node")
	}
}

func TestTree_Walk(t *testing.T) {
	input := "# A\n## B\n- [ ] C\n"
	tree := Parse(input)

	var titles []string
	tree.Walk(func(n *Node) bool {
		titles = append(titles, n.Title)
		return true
	})

	want := []string{"A", "B", "C"}
	if len(titles) != len(want) {
		t.Fatalf("Walk visited %d nodes, want %d", len(titles), len(want))
	}
	for i, title := range titles {
		if title != want[i] {
			t.Errorf("titles[%d] = %q, want %q", i, title, want[i])
		}
	}
}

func TestTree_Walk_SkipChildren(t *testing.T) {
	input := "# A\n## B\n- [ ] C\n# D\n"
	tree := Parse(input)

	var titles []string
	tree.Walk(func(n *Node) bool {
		titles = append(titles, n.Title)
		// Skip children of A.
		return n.Title != "A"
	})

	// A is visited but its children (B, C) are skipped. D is a sibling root.
	want := []string{"A", "D"}
	if len(titles) != len(want) {
		t.Fatalf("Walk visited %d nodes, want %d", len(titles), len(want))
	}
	for i, title := range titles {
		if title != want[i] {
			t.Errorf("titles[%d] = %q, want %q", i, title, want[i])
		}
	}
}

func TestTree_AncestryPath(t *testing.T) {
	input := "# Root\n## Child\n### Grandchild\n- [ ] Leaf\n"
	tree := Parse(input)

	path := tree.AncestryPath("Leaf")
	want := "Root > Child > Grandchild > Leaf"
	if path != want {
		t.Errorf("AncestryPath = %q, want %q", path, want)
	}
}

func TestTree_AncestryPath_NotFound(t *testing.T) {
	tree := Parse("# A\n")
	path := tree.AncestryPath("nonexistent")
	if path != "" {
		t.Errorf("AncestryPath = %q, want empty", path)
	}
}

func TestTree_AncestryPath_Root(t *testing.T) {
	tree := Parse("# Root\n")
	path := tree.AncestryPath("Root")
	if path != "Root" {
		t.Errorf("AncestryPath = %q, want %q", path, "Root")
	}
}
