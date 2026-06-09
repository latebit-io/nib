package ui

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// TreeNode represents a file or directory in a collapsible tree.
type TreeNode struct {
	// Name is the display name (filename or directory name).
	Name string
	// Path is the relative path from the project root (forward slashes).
	Path string
	// IsDir is true for directory nodes.
	IsDir bool
	// Collapsed is true when a directory's children are hidden.
	Collapsed bool
	// Badge is an optional right-aligned label ("ctx", "mod").
	Badge string
	// Children holds child nodes, sorted: directories first, then files, alphabetical.
	Children []*TreeNode
	parent   *TreeNode // back-pointer for depth calculation
}

// Depth returns the nesting level (0 for top-level entries).
// The synthetic root node is not counted.
func (n *TreeNode) Depth() int {
	d := 0
	for p := n.parent; p != nil && p.parent != nil; p = p.parent {
		d++
	}
	return d
}

// Toggle flips the collapsed state of a directory node.
// No-op on file nodes.
func (n *TreeNode) Toggle() {
	if n.IsDir {
		n.Collapsed = !n.Collapsed
	}
}

// BuildTree converts a sorted list of relative file paths into a tree.
// All intermediate directories are created automatically. Paths are
// normalized to forward slashes so OS-native separators are handled.
// The returned root is a synthetic node (Name="", IsDir=true) whose
// children are the top-level entries.
func BuildTree(paths []string) *TreeNode {
	root := &TreeNode{IsDir: true}
	for _, p := range paths {
		p = filepath.ToSlash(p)
		parts := strings.Split(p, "/")
		insertPath(root, parts, p)
	}
	sortChildren(root)
	return root
}

// insertPath walks or creates intermediate directory nodes and inserts the
// leaf file node.
func insertPath(parent *TreeNode, parts []string, fullPath string) {
	if len(parts) == 0 {
		return
	}
	name := parts[0]
	isLeaf := len(parts) == 1

	if isLeaf {
		parent.Children = append(parent.Children, &TreeNode{
			Name:   name,
			Path:   fullPath,
			IsDir:  false,
			parent: parent,
		})
		return
	}

	// Find or create directory
	var dir *TreeNode
	for _, c := range parent.Children {
		if c.IsDir && c.Name == name {
			dir = c
			break
		}
	}
	if dir == nil {
		dir = &TreeNode{
			Name:      name,
			Path:      path.Join(parent.Path, name),
			IsDir:     true,
			Collapsed: true,
			parent:    parent,
		}
		parent.Children = append(parent.Children, dir)
	}
	insertPath(dir, parts[1:], fullPath)
}

// InsertDir ensures a directory node exists at the given relative path.
// Creates intermediate directories as needed. The path is normalized
// to forward slashes. Returns the directory node.
func InsertDir(root *TreeNode, relPath string) *TreeNode {
	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")
	node := root
	for _, name := range parts {
		if name == "" {
			continue
		}
		var child *TreeNode
		for _, c := range node.Children {
			if c.IsDir && c.Name == name {
				child = c
				break
			}
		}
		if child == nil {
			child = &TreeNode{
				Name:      name,
				Path:      path.Join(node.Path, name),
				IsDir:     true,
				Collapsed: true,
				parent:    node,
			}
			node.Children = append(node.Children, child)
		}
		node = child
	}
	return node
}

// sortChildren recursively sorts: directories first, then files,
// alphabetical within each group.
func sortChildren(n *TreeNode) {
	sort.Slice(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir // dirs before files
		}
		return a.Name < b.Name
	})
	for _, c := range n.Children {
		if c.IsDir {
			sortChildren(c)
		}
	}
}

// FlattenVisible returns nodes in display order, skipping children
// of collapsed directories. The synthetic root is not included.
func FlattenVisible(root *TreeNode) []*TreeNode {
	if root == nil {
		return nil
	}
	var result []*TreeNode
	for _, c := range root.Children {
		flattenNode(c, &result)
	}
	return result
}

func flattenNode(n *TreeNode, result *[]*TreeNode) {
	*result = append(*result, n)
	if n.IsDir && !n.Collapsed {
		for _, c := range n.Children {
			flattenNode(c, result)
		}
	}
}

// SetBadges updates badges on tree nodes based on the set of agent-modified files.
// modifiedFiles should be relative paths matching TreeNode.Path.
func SetBadges(root *TreeNode, modifiedFiles map[string]bool) {
	if root == nil {
		return
	}
	walkTree(root, func(n *TreeNode) {
		if n.IsDir {
			n.Badge = ""
			return
		}
		if modifiedFiles[n.Path] {
			n.Badge = "mod"
		} else {
			n.Badge = ""
		}
	})
}

// walkTree visits every node in the tree.
func walkTree(n *TreeNode, fn func(*TreeNode)) {
	fn(n)
	for _, c := range n.Children {
		walkTree(c, fn)
	}
}

// ExpandedPaths returns the set of directory paths that are currently expanded.
func ExpandedPaths(root *TreeNode) map[string]bool {
	expanded := make(map[string]bool)
	if root == nil {
		return expanded
	}
	walkTree(root, func(n *TreeNode) {
		if n.IsDir && !n.Collapsed {
			expanded[n.Path] = true
		}
	})
	return expanded
}

// RestoreExpanded expands directories whose paths are in the given set.
func RestoreExpanded(root *TreeNode, expanded map[string]bool) {
	if root == nil {
		return
	}
	walkTree(root, func(n *TreeNode) {
		if n.IsDir && expanded[n.Path] {
			n.Collapsed = false
		}
	})
}

// ExpandToPath expands all ancestor directories of the given file path
// so that the file (or deepest directory) becomes visible in the flattened
// tree. The path should be relative with forward slashes, matching
// TreeNode.Path conventions.
func ExpandToPath(root *TreeNode, filePath string) {
	if root == nil || filePath == "" {
		return
	}
	filePath = filepath.ToSlash(filePath)
	// Walk the tree following each path segment, expanding dirs along the way.
	parts := strings.Split(filePath, "/")
	node := root
	for i, name := range parts {
		var child *TreeNode
		for _, c := range node.Children {
			if c.Name == name {
				child = c
				break
			}
		}
		if child == nil {
			return // path not in tree
		}
		if child.IsDir {
			child.Collapsed = false
		}
		// For the last segment (the file itself), nothing more to do.
		if i == len(parts)-1 {
			return
		}
		node = child
	}
}

// findNode returns the first node with the given relative path, or nil.
func findNode(root *TreeNode, relPath string) *TreeNode {
	if root == nil {
		return nil
	}
	return findNodeRecursive(root, relPath)
}

func findNodeRecursive(n *TreeNode, relPath string) *TreeNode {
	if n.Path == relPath {
		return n
	}
	for _, c := range n.Children {
		if found := findNodeRecursive(c, relPath); found != nil {
			return found
		}
	}
	return nil
}
