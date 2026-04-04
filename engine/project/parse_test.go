package project

import (
	"testing"
)

func TestParse_Frontmatter(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantPrj string
	}{
		{
			name:    "extracts project name",
			input:   "---\nproject: Junto\n---\n# Component",
			wantPrj: "Junto",
		},
		{
			name:    "no frontmatter",
			input:   "# Component",
			wantPrj: "",
		},
		{
			name:    "frontmatter with extra fields",
			input:   "---\nproject: Junto\nversion: 1\n---\n# Component",
			wantPrj: "Junto",
		},
		{
			name:    "empty frontmatter",
			input:   "---\n---\n# Component",
			wantPrj: "",
		},
		{
			name:    "frontmatter with spaces in value",
			input:   "---\nproject: My Cool Project\n---\n",
			wantPrj: "My Cool Project",
		},
		{
			name:    "unterminated frontmatter ignores fields",
			input:   "---\nproject: Junto\n# Component",
			wantPrj: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree := Parse(tt.input)
			if tree.ProjectName != tt.wantPrj {
				t.Errorf("ProjectName = %q, want %q", tree.ProjectName, tt.wantPrj)
			}
		})
	}
}

func TestParse_UnterminatedFrontmatter_PreservesContent(t *testing.T) {
	input := "---\nproject: Junto\n# Component\n- [ ] task"
	tree := Parse(input)

	if tree.ProjectName != "" {
		t.Errorf("ProjectName = %q, want empty (unterminated frontmatter)", tree.ProjectName)
	}
	// The heading and task should be parsed, not swallowed.
	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1 (content preserved after rewind)", len(tree.Roots))
	}
	assertHeading(t, tree.Roots[0], "Component", 0)
	if len(tree.Roots[0].Children) != 1 {
		t.Fatalf("Children = %d, want 1", len(tree.Roots[0].Children))
	}
	assertTask(t, tree.Roots[0].Children[0], "task", TaskPending, 1)
}

func TestParse_Headings(t *testing.T) {
	input := "# Component\n## Phase\n### Feature\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}

	root := tree.Roots[0]
	assertHeading(t, root, "Component", 0)

	if len(root.Children) != 1 {
		t.Fatalf("Component.Children = %d, want 1", len(root.Children))
	}
	phase := root.Children[0]
	assertHeading(t, phase, "Phase", 1)

	if len(phase.Children) != 1 {
		t.Fatalf("Phase.Children = %d, want 1", len(phase.Children))
	}
	feature := phase.Children[0]
	assertHeading(t, feature, "Feature", 2)
}

func TestParse_SiblingHeadings(t *testing.T) {
	input := "# A\n## B\n## C\n# D\n"
	tree := Parse(input)

	if len(tree.Roots) != 2 {
		t.Fatalf("Roots = %d, want 2", len(tree.Roots))
	}

	assertHeading(t, tree.Roots[0], "A", 0)
	assertHeading(t, tree.Roots[1], "D", 0)

	if len(tree.Roots[0].Children) != 2 {
		t.Fatalf("A.Children = %d, want 2", len(tree.Roots[0].Children))
	}
	assertHeading(t, tree.Roots[0].Children[0], "B", 1)
	assertHeading(t, tree.Roots[0].Children[1], "C", 1)
}

func TestParse_DepthJump(t *testing.T) {
	// h1 → h3 (skipping h2) — h3 should still nest under h1
	input := "# Root\n### Deep\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	root := tree.Roots[0]
	if len(root.Children) != 1 {
		t.Fatalf("Root.Children = %d, want 1", len(root.Children))
	}
	assertHeading(t, root.Children[0], "Deep", 2)
}

func TestParse_Tasks(t *testing.T) {
	input := "# Feature\n- [ ] pending\n- [x] done\n- [>] active\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	feature := tree.Roots[0]
	if len(feature.Children) != 3 {
		t.Fatalf("Feature.Children = %d, want 3", len(feature.Children))
	}

	assertTask(t, feature.Children[0], "pending", TaskPending, 1)
	assertTask(t, feature.Children[1], "done", TaskDone, 1)
	assertTask(t, feature.Children[2], "active", TaskActive, 1)
}

func TestParse_TasksWithoutHeading(t *testing.T) {
	input := "- [ ] orphan task\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	assertTask(t, tree.Roots[0], "orphan task", TaskPending, 0)
}

func TestParse_MixedContent(t *testing.T) {
	input := `---
project: Junto
---

# TUI Editor + Agent

Some prose that should be ignored.

## Phase 6: Plans

More prose here.

### Planning Mode
- [>] Add Phase to Session
- [ ] Tool filtering

### Breakdown View
- [ ] Parse project.md

## Phase 7: Plugin System
`
	tree := Parse(input)

	if tree.ProjectName != "Junto" {
		t.Errorf("ProjectName = %q, want %q", tree.ProjectName, "Junto")
	}
	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}

	comp := tree.Roots[0]
	assertHeading(t, comp, "TUI Editor + Agent", 0)
	if len(comp.Children) != 2 {
		t.Fatalf("Component.Children = %d, want 2", len(comp.Children))
	}

	phase6 := comp.Children[0]
	assertHeading(t, phase6, "Phase 6: Plans", 1)
	if len(phase6.Children) != 2 {
		t.Fatalf("Phase6.Children = %d, want 2", len(phase6.Children))
	}

	planning := phase6.Children[0]
	assertHeading(t, planning, "Planning Mode", 2)
	if len(planning.Children) != 2 {
		t.Fatalf("Planning.Children = %d, want 2", len(planning.Children))
	}
	assertTask(t, planning.Children[0], "Add Phase to Session", TaskActive, 3)
	assertTask(t, planning.Children[1], "Tool filtering", TaskPending, 3)

	breakdown := phase6.Children[1]
	assertHeading(t, breakdown, "Breakdown View", 2)
	if len(breakdown.Children) != 1 {
		t.Fatalf("Breakdown.Children = %d, want 1", len(breakdown.Children))
	}
	assertTask(t, breakdown.Children[0], "Parse project.md", TaskPending, 3)

	phase7 := comp.Children[1]
	assertHeading(t, phase7, "Phase 7: Plugin System", 1)
	if len(phase7.Children) != 0 {
		t.Fatalf("Phase7.Children = %d, want 0", len(phase7.Children))
	}
}

func TestParse_EmptyInput(t *testing.T) {
	tree := Parse("")
	if len(tree.Roots) != 0 {
		t.Errorf("Roots = %d, want 0", len(tree.Roots))
	}
	if tree.ProjectName != "" {
		t.Errorf("ProjectName = %q, want empty", tree.ProjectName)
	}
}

func TestParse_InvalidHeadings(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"no space after hash", "#NoSpace"},
		{"empty title", "# "},
		{"seven hashes", "####### too deep"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree := Parse(tt.input)
			if len(tree.Roots) != 0 {
				t.Errorf("Roots = %d, want 0 for invalid heading %q", len(tree.Roots), tt.input)
			}
		})
	}
}

func TestParse_EmptyTaskTitle(t *testing.T) {
	input := "# H\n- [ ] \n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	if len(tree.Roots[0].Children) != 0 {
		t.Errorf("Children = %d, want 0 (empty task title rejected)", len(tree.Roots[0].Children))
	}
}

func TestParse_FencedCodeBlockIgnored(t *testing.T) {
	input := "# Real\n```\n# Fake Heading\n- [ ] fake task\n```\n- [ ] real task\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	root := tree.Roots[0]
	assertHeading(t, root, "Real", 0)
	if len(root.Children) != 1 {
		t.Fatalf("Children = %d, want 1 (fenced content should be ignored)", len(root.Children))
	}
	assertTask(t, root.Children[0], "real task", TaskPending, 1)
}

func TestParse_TildeFenceIgnored(t *testing.T) {
	input := "# A\n~~~\n## Fake\n~~~\n- [ ] real\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	if len(tree.Roots[0].Children) != 1 {
		t.Fatalf("Children = %d, want 1", len(tree.Roots[0].Children))
	}
	assertTask(t, tree.Roots[0].Children[0], "real", TaskPending, 1)
}

func TestParse_FencedCodeWithLanguageTag(t *testing.T) {
	input := "# A\n```markdown\n# Fake\n- [ ] fake\n```\n- [ ] real\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	if len(tree.Roots[0].Children) != 1 {
		t.Fatalf("Children = %d, want 1 (fenced with language tag should be ignored)", len(tree.Roots[0].Children))
	}
	assertTask(t, tree.Roots[0].Children[0], "real", TaskPending, 1)
}

func TestParse_FenceMixedDelimiters(t *testing.T) {
	// ~~~ inside a backtick fence must not close it.
	input := "# A\n```\n~~~\n# Fake\n~~~\n```\n- [ ] real\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	if len(tree.Roots[0].Children) != 1 {
		t.Fatalf("Children = %d, want 1 (mixed delimiters should not close fence)", len(tree.Roots[0].Children))
	}
	assertTask(t, tree.Roots[0].Children[0], "real", TaskPending, 1)
}

func TestParse_FenceLongerCloser(t *testing.T) {
	// A 4-backtick fence must not be closed by 3 backticks.
	input := "# A\n````\n```\n# Fake\n```\n````\n- [ ] real\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	if len(tree.Roots[0].Children) != 1 {
		t.Fatalf("Children = %d, want 1 (shorter fence should not close longer opener)", len(tree.Roots[0].Children))
	}
	assertTask(t, tree.Roots[0].Children[0], "real", TaskPending, 1)
}

func TestParse_IndentedTasks(t *testing.T) {
	input := "# H\n  - [ ] indented task\n"
	tree := Parse(input)

	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
	if len(tree.Roots[0].Children) != 1 {
		t.Fatalf("Children = %d, want 1", len(tree.Roots[0].Children))
	}
	assertTask(t, tree.Roots[0].Children[0], "indented task", TaskPending, 1)
}

func TestParseHeading(t *testing.T) {
	tests := []struct {
		line      string
		wantLevel int
		wantTitle string
		wantOK    bool
	}{
		{"# Title", 1, "Title", true},
		{"## Phase 2", 2, "Phase 2", true},
		{"###### Deep", 6, "Deep", true},
		{"#NoSpace", 0, "", false},
		{"# ", 0, "", false},
		{"####### Seven", 0, "", false},
		{"not a heading", 0, "", false},
		{"", 0, "", false},
		{"  ## Indented", 2, "Indented", true},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			level, title, ok := parseHeading(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if level != tt.wantLevel {
				t.Errorf("level = %d, want %d", level, tt.wantLevel)
			}
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
		})
	}
}

func TestParseTaskItem(t *testing.T) {
	tests := []struct {
		line       string
		wantTitle  string
		wantStatus TaskStatus
		wantOK     bool
	}{
		{"- [ ] pending", "pending", TaskPending, true},
		{"- [x] done", "done", TaskDone, true},
		{"- [>] active", "active", TaskActive, true},
		{"  - [ ] indented", "indented", TaskPending, true},
		{"- [ ] ", "", 0, false},
		{"- not a task", "", 0, false},
		{"* [ ] wrong bullet", "", 0, false},
		{"", "", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			title, status, ok := parseTaskItem(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v", status, tt.wantStatus)
			}
		})
	}
}

func assertHeading(t *testing.T, n *Node, title string, depth int) {
	t.Helper()
	if n.Title != title {
		t.Errorf("Title = %q, want %q", n.Title, title)
	}
	if n.Depth != depth {
		t.Errorf("Depth = %d, want %d (node %q)", n.Depth, depth, title)
	}
	if !n.IsHeading {
		t.Errorf("IsHeading = false, want true (node %q)", title)
	}
}

func assertTask(t *testing.T, n *Node, title string, status TaskStatus, depth int) {
	t.Helper()
	if n.Title != title {
		t.Errorf("Title = %q, want %q", n.Title, title)
	}
	if n.Status != status {
		t.Errorf("Status = %v, want %v (node %q)", n.Status, status, title)
	}
	if n.Depth != depth {
		t.Errorf("Depth = %d, want %d (node %q)", n.Depth, depth, title)
	}
	if n.IsHeading {
		t.Errorf("IsHeading = true, want false (node %q)", title)
	}
}
