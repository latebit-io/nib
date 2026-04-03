package project

import (
	"strings"
)

// Parse converts a markdown document into a work tree.
// The document may include YAML frontmatter delimited by "---" lines
// containing a "project:" field. Headings (# through ######) become
// hierarchy nodes; task-list items (- [ ], - [x], - [>]) become leaf
// tasks beneath their nearest heading ancestor.
//
// Lines that are not headings, task items, or frontmatter are ignored.
// This keeps the parser focused on structure and tolerant of prose,
// blank lines, and other markdown content.
func Parse(markdown string) *Tree {
	lines := strings.Split(markdown, "\n")
	tree := &Tree{}

	i := 0

	// Extract frontmatter if present.
	if i < len(lines) && strings.TrimSpace(lines[i]) == "---" {
		i++
		for i < len(lines) {
			line := strings.TrimSpace(lines[i])
			i++
			if line == "---" {
				break
			}
			if key, val, ok := parseYAMLField(line); ok && key == "project" {
				tree.ProjectName = val
			}
		}
	}

	// headingStack tracks the most recent heading at each depth (0-indexed).
	// When a new heading appears at depth d, it becomes a child of the
	// deepest heading with depth < d (its nearest ancestor).
	var headingStack []*Node

	for ; i < len(lines); i++ {
		line := lines[i]

		if level, title, ok := parseHeading(line); ok {
			node := &Node{
				Title:     title,
				Depth:     level - 1, // h1→0, h2→1, etc.
				IsHeading: true,
			}
			attachHeading(tree, &headingStack, node)
			continue
		}

		if title, status, ok := parseTaskItem(line); ok {
			node := &Node{
				Title:     title,
				IsHeading: false,
				Status:    status,
			}
			attachTask(tree, &headingStack, node)
		}
	}

	return tree
}

// attachHeading inserts a heading node into the tree at the correct depth.
// It maintains headingStack so that subsequent nodes can find their parent.
func attachHeading(tree *Tree, stack *[]*Node, node *Node) {
	// Find the nearest ancestor: the deepest stack entry with depth < node.Depth.
	parent := findParent(*stack, node.Depth)

	if parent == nil {
		tree.Roots = append(tree.Roots, node)
	} else {
		parent.Children = append(parent.Children, node)
	}

	// Trim the stack to remove any entries at or below this depth,
	// then push this node as the current entry at its depth.
	var trimmed []*Node
	for _, s := range *stack {
		if s.Depth < node.Depth {
			trimmed = append(trimmed, s)
		}
	}
	*stack = append(trimmed, node)
}

// attachTask inserts a task node under the most recent heading,
// or as a root-level node if no headings have been seen.
func attachTask(tree *Tree, stack *[]*Node, node *Node) {
	if len(*stack) > 0 {
		parent := (*stack)[len(*stack)-1]
		node.Depth = parent.Depth + 1
		parent.Children = append(parent.Children, node)
	} else {
		node.Depth = 0
		tree.Roots = append(tree.Roots, node)
	}
}

// findParent returns the deepest node in the stack with depth < targetDepth.
func findParent(stack []*Node, targetDepth int) *Node {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].Depth < targetDepth {
			return stack[i]
		}
	}
	return nil
}

// parseHeading extracts the level and title from a markdown heading line.
// Returns (level, title, true) for valid headings, or (0, "", false) otherwise.
func parseHeading(line string) (int, string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}

	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}

	rest := trimmed[level:]
	if len(rest) == 0 || rest[0] != ' ' {
		return 0, "", false
	}

	title := strings.TrimSpace(rest)
	if title == "" {
		return 0, "", false
	}

	return level, title, true
}

// parseTaskItem extracts the title and status from a markdown task-list item.
// Supported formats: "- [ ] text", "- [x] text", "- [>] text".
// Leading whitespace is tolerated for indented items.
func parseTaskItem(line string) (string, TaskStatus, bool) {
	trimmed := strings.TrimSpace(line)

	for _, tp := range taskPrefixes {
		if strings.HasPrefix(trimmed, tp.prefix) {
			title := strings.TrimSpace(trimmed[len(tp.prefix):])
			if title == "" {
				return "", 0, false
			}
			return title, tp.status, true
		}
	}
	return "", 0, false
}

// taskPrefix pairs a markdown task-list prefix with its status.
type taskPrefix struct {
	prefix string
	status TaskStatus
}

// taskPrefixes defines the recognized task-list markers in match order.
// Using an ordered slice (not a map) guarantees deterministic matching.
var taskPrefixes = []taskPrefix{
	{"- [ ] ", TaskPending},
	{"- [x] ", TaskDone},
	{"- [>] ", TaskActive},
}

// parseYAMLField extracts a simple "key: value" pair from a YAML line.
// Only handles flat scalar fields — no nested structures.
func parseYAMLField(line string) (string, string, bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}
