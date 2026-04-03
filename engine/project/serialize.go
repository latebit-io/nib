package project

import (
	"strings"
)

// Serialize converts a work tree back into markdown text.
// The output is structurally equivalent to the input that produced
// the tree via Parse — frontmatter, headings, and task items are
// emitted in depth-first order.
//
// Content that was not parsed (prose, blank lines, comments) is not
// preserved. This is a structural round-trip: parse → modify → serialize
// preserves the work hierarchy, not arbitrary markdown.
func Serialize(tree *Tree) string {
	if tree == nil {
		return ""
	}

	var b strings.Builder

	if tree.ProjectName != "" {
		b.WriteString("---\n")
		b.WriteString("project: ")
		b.WriteString(tree.ProjectName)
		b.WriteString("\n---\n")
	}

	for i, root := range tree.Roots {
		if i > 0 || tree.ProjectName != "" {
			b.WriteString("\n")
		}
		serializeNode(&b, root)
	}

	return b.String()
}

// serializeNode writes a single node and its children.
func serializeNode(b *strings.Builder, n *Node) {
	if n.IsHeading {
		serializeHeading(b, n)
	} else {
		serializeTask(b, n)
	}

	for _, child := range n.Children {
		serializeNode(b, child)
	}
}

// serializeHeading writes a markdown heading at the correct level.
func serializeHeading(b *strings.Builder, n *Node) {
	level := max(n.Depth+1, 1) // depth 0 → h1
	level = min(level, 6)

	b.WriteString(strings.Repeat("#", level))
	b.WriteString(" ")
	b.WriteString(n.Title)
	b.WriteString("\n")
}

// serializeTask writes a markdown task-list item with the appropriate status marker.
func serializeTask(b *strings.Builder, n *Node) {
	var marker string
	switch n.Status {
	case TaskDone:
		marker = "- [x] "
	case TaskActive:
		marker = "- [>] "
	default:
		marker = "- [ ] "
	}
	b.WriteString(marker)
	b.WriteString(n.Title)
	b.WriteString("\n")
}
