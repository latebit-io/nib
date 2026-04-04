// Package project parses and serializes structured work hierarchies from
// markdown documents. A work tree represents the project's components,
// phases, features, and goals as a navigable hierarchy derived from
// markdown heading levels and task-list items.
//
// The canonical source document uses this structure:
//
//	---
//	project: My Project
//	---
//	# Component
//	## Phase
//	### Feature
//	- [ ] pending goal
//	- [x] completed goal
//	- [>] active goal
//
// Heading depth maps to tree depth (h1 = depth 0, h2 = depth 1, etc.).
// Task-list items are leaf nodes beneath their nearest heading ancestor.
package project

import "strings"

// TaskStatus represents the completion state of a goal item.
type TaskStatus int

const (
	// TaskPending indicates work not yet started.
	TaskPending TaskStatus = iota
	// TaskActive indicates the currently in-progress goal.
	TaskActive
	// TaskDone indicates completed work.
	TaskDone
)

// Node is a single element in the work hierarchy.
// Heading nodes (IsHeading=true) represent components, phases, or features.
// Task nodes (IsHeading=false) represent individual goals.
type Node struct {
	// Title is the display text (heading text or task description).
	Title string
	// Depth is the nesting level: h1→0, h2→1, h3→2, etc.
	// Task nodes inherit the depth of their parent heading + 1.
	Depth int
	// IsHeading distinguishes structural headings from leaf tasks.
	IsHeading bool
	// Status is the task completion state. Only meaningful when IsHeading is false.
	Status TaskStatus
	// Children contains nested headings and tasks.
	Children []*Node
}

// Tree is the root container for a parsed work hierarchy.
type Tree struct {
	// ProjectName is extracted from the YAML frontmatter "project:" field.
	// Empty if no frontmatter is present.
	ProjectName string
	// Roots are the top-level nodes (h1 headings).
	Roots []*Node
}

// ActiveGoal returns the first task node with TaskActive status,
// along with the ancestry path from root to that node (inclusive).
// Returns nil, nil if no active goal exists.
func (t *Tree) ActiveGoal() (goal *Node, ancestry []*Node) {
	for _, root := range t.Roots {
		if g, path := findActive(root, nil); g != nil {
			return g, path
		}
	}
	return nil, nil
}

// SetActiveGoal marks the task at targetTitle as active and clears any
// previously active task. Returns true if the target was found and updated.
// Only leaf tasks (IsHeading=false) can be set active. If the target is
// not found, the tree is left unchanged.
func (t *Tree) SetActiveGoal(targetTitle string) bool {
	// Find and validate target before mutating any state.
	var target *Node
	for _, root := range t.Roots {
		if n := findTaskByTitle(root, targetTitle); n != nil {
			target = n
			break
		}
	}
	if target == nil {
		return false
	}

	// Target confirmed — safe to clear previous active and set new one.
	for _, root := range t.Roots {
		clearActive(root)
	}
	target.Status = TaskActive
	return true
}

// Walk calls fn for every node in depth-first order.
// If fn returns false, traversal of that node's children is skipped.
func (t *Tree) Walk(fn func(n *Node) bool) {
	for _, root := range t.Roots {
		walkNode(root, fn)
	}
}

// AncestryPath returns the title of each node from root to the target,
// formatted as "Root > Child > Grandchild". The target node itself is
// included. Returns empty string if the target is not found.
func (t *Tree) AncestryPath(targetTitle string) string {
	for _, root := range t.Roots {
		if path := buildAncestry(root, targetTitle, nil); path != nil {
			titles := make([]string, len(path))
			for i, n := range path {
				titles[i] = n.Title
			}
			return strings.Join(titles, " > ")
		}
	}
	return ""
}

// findActive searches depth-first for the first TaskActive node.
func findActive(n *Node, path []*Node) (*Node, []*Node) {
	path = append(path, n)
	if !n.IsHeading && n.Status == TaskActive {
		return n, path
	}
	for _, child := range n.Children {
		if g, p := findActive(child, path); g != nil {
			return g, p
		}
	}
	return nil, nil
}

// clearActive resets all TaskActive statuses to TaskPending.
func clearActive(n *Node) {
	if !n.IsHeading && n.Status == TaskActive {
		n.Status = TaskPending
	}
	for _, child := range n.Children {
		clearActive(child)
	}
}

// findTaskByTitle returns the first leaf task node (IsHeading=false) with
// the given title. Heading nodes are not matched.
func findTaskByTitle(n *Node, title string) *Node {
	if !n.IsHeading && n.Title == title {
		return n
	}
	for _, child := range n.Children {
		if found := findTaskByTitle(child, title); found != nil {
			return found
		}
	}
	return nil
}

// walkNode recursively visits n and its children.
func walkNode(n *Node, fn func(*Node) bool) {
	if !fn(n) {
		return
	}
	for _, child := range n.Children {
		walkNode(child, fn)
	}
}

// buildAncestry returns the path from root to the node with targetTitle,
// or nil if not found.
func buildAncestry(n *Node, targetTitle string, path []*Node) []*Node {
	path = append(path, n)
	if n.Title == targetTitle {
		return path
	}
	for _, child := range n.Children {
		if result := buildAncestry(child, targetTitle, path); result != nil {
			return result
		}
	}
	return nil
}
