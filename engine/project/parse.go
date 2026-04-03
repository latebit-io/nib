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

	i := parseFrontmatter(lines, tree)

	// headingStack tracks the most recent heading at each depth (0-indexed).
	// When a new heading appears at depth d, it becomes a child of the
	// deepest heading with depth < d (its nearest ancestor).
	var headingStack []*Node
	var fenceChar byte // '`' or '~' when inside a fence, 0 when outside
	var fenceLen int   // minimum closer length (>= opener length)

	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Track fenced code blocks per CommonMark: a closing fence must
		// use the same character as the opener with at least as many chars.
		if fc, fl, ok := parseFence(trimmed); ok {
			if fenceChar == 0 {
				// Enter fence.
				fenceChar = fc
				fenceLen = fl
				continue
			}
			if fc == fenceChar && fl >= fenceLen {
				// Matching closer — exit fence.
				fenceChar = 0
				fenceLen = 0
				continue
			}
		}
		if fenceChar != 0 {
			continue
		}

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

// parseFrontmatter extracts YAML frontmatter from the beginning of lines.
// Returns the line index where content parsing should begin.
// If the closing "---" delimiter is missing, the opening delimiter is not
// treated as frontmatter and the returned index is 0 to avoid losing content.
func parseFrontmatter(lines []string, tree *Tree) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}

	var projectName string
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "---" {
			tree.ProjectName = projectName
			return i + 1
		}
		if key, val, ok := parseYAMLField(line); ok && key == "project" {
			projectName = val
		}
	}
	return 0 // unterminated — rewind
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

	// Pop entries at or below this depth, then push. The stack is ordered
	// by depth so we only need to trim from the tail — no allocation.
	cut := len(*stack)
	for cut > 0 && (*stack)[cut-1].Depth >= node.Depth {
		cut--
	}
	*stack = append((*stack)[:cut], node)
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

// parseFence detects a fenced code block delimiter (``` or ~~~).
// Returns the fence character, its count, and true if the line starts with
// 3+ identical backticks or tildes. Handles language tags (e.g. ```go).
func parseFence(trimmed string) (byte, int, bool) {
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	count := 1
	for count < len(trimmed) && trimmed[count] == ch {
		count++
	}
	if count < 3 {
		return 0, 0, false
	}
	return ch, count, true
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
