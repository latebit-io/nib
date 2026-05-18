package ui

import (
	"testing"
)

func TestBuildTree_Empty(t *testing.T) {
	root := BuildTree(nil)
	if root == nil {
		t.Fatal("expected non-nil root")
	}
	if len(root.Children) != 0 {
		t.Errorf("expected 0 children, got %d", len(root.Children))
	}
}

func TestBuildTree_FlatFiles(t *testing.T) {
	root := BuildTree([]string{"a.go", "b.go", "c.go"})
	if len(root.Children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(root.Children))
	}
	for i, name := range []string{"a.go", "b.go", "c.go"} {
		c := root.Children[i]
		if c.Name != name {
			t.Errorf("child[%d]: expected %q, got %q", i, name, c.Name)
		}
		if c.IsDir {
			t.Errorf("child[%d]: expected file, got dir", i)
		}
		if c.Path != name {
			t.Errorf("child[%d]: expected path %q, got %q", i, name, c.Path)
		}
	}
}

func TestBuildTree_NestedPaths(t *testing.T) {
	root := BuildTree([]string{
		"cmd/main.go",
		"internal/auth/handler.go",
		"internal/auth/token.go",
		"internal/payments/handler.go",
		"go.mod",
	})

	// Top-level: dirs first (cmd, internal), then files (go.mod)
	if len(root.Children) != 3 {
		t.Fatalf("expected 3 top-level children, got %d", len(root.Children))
	}
	if root.Children[0].Name != "cmd" || !root.Children[0].IsDir {
		t.Errorf("expected dir 'cmd', got %q isDir=%v", root.Children[0].Name, root.Children[0].IsDir)
	}
	if root.Children[1].Name != "internal" || !root.Children[1].IsDir {
		t.Errorf("expected dir 'internal', got %q isDir=%v", root.Children[1].Name, root.Children[1].IsDir)
	}
	if root.Children[2].Name != "go.mod" || root.Children[2].IsDir {
		t.Errorf("expected file 'go.mod', got %q isDir=%v", root.Children[2].Name, root.Children[2].IsDir)
	}

	// internal/auth should have 2 files
	auth := root.Children[1].Children[0] // internal -> auth
	if auth.Name != "auth" {
		t.Fatalf("expected 'auth', got %q", auth.Name)
	}
	if len(auth.Children) != 2 {
		t.Fatalf("expected 2 children in auth, got %d", len(auth.Children))
	}
}

func TestBuildTree_DirsDefaultCollapsed(t *testing.T) {
	root := BuildTree([]string{"src/main.go"})
	dir := root.Children[0]
	if !dir.IsDir {
		t.Fatal("expected dir")
	}
	if !dir.Collapsed {
		t.Error("directories should default to collapsed")
	}
}

func TestDepth(t *testing.T) {
	root := BuildTree([]string{"a/b/c.go"})
	// a (depth 1), b (depth 2), c.go (depth 3)
	// But root children start at depth 0 from the root's perspective.
	// Depth counts parent hops excluding root.
	a := root.Children[0]
	b := a.Children[0]
	c := b.Children[0]

	tests := []struct {
		name string
		node *TreeNode
		want int
	}{
		{"top-level dir", a, 0},
		{"nested dir", b, 1},
		{"leaf file", c, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.node.Depth()
			if got != tt.want {
				t.Errorf("Depth() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFlattenVisible_AllCollapsed(t *testing.T) {
	root := BuildTree([]string{
		"cmd/main.go",
		"internal/auth/handler.go",
		"README.md",
	})
	// All dirs default to collapsed, so only top-level items visible
	flat := FlattenVisible(root)
	names := nodeNames(flat)
	want := []string{"cmd", "internal", "README.md"}
	assertNames(t, names, want)
}

func TestFlattenVisible_Expanded(t *testing.T) {
	root := BuildTree([]string{
		"cmd/main.go",
		"internal/auth/handler.go",
		"README.md",
	})

	// Expand cmd
	root.Children[0].Collapsed = false
	flat := FlattenVisible(root)
	names := nodeNames(flat)
	want := []string{"cmd", "main.go", "internal", "README.md"}
	assertNames(t, names, want)
}

func TestFlattenVisible_DeepExpand(t *testing.T) {
	root := BuildTree([]string{
		"a/b/c.go",
		"a/b/d.go",
	})

	// Expand a but not b
	root.Children[0].Collapsed = false
	flat := FlattenVisible(root)
	names := nodeNames(flat)
	want := []string{"a", "b"}
	assertNames(t, names, want)

	// Now expand b too
	root.Children[0].Children[0].Collapsed = false
	flat = FlattenVisible(root)
	names = nodeNames(flat)
	want = []string{"a", "b", "c.go", "d.go"}
	assertNames(t, names, want)
}

func TestToggle(t *testing.T) {
	root := BuildTree([]string{"src/main.go"})
	dir := root.Children[0]

	if !dir.Collapsed {
		t.Fatal("should start collapsed")
	}
	dir.Toggle()
	if dir.Collapsed {
		t.Error("should be expanded after toggle")
	}
	dir.Toggle()
	if !dir.Collapsed {
		t.Error("should be collapsed after second toggle")
	}
}

func TestToggle_FileNoop(t *testing.T) {
	root := BuildTree([]string{"main.go"})
	file := root.Children[0]
	file.Toggle() // should not panic
	if file.IsDir {
		t.Error("file should not become dir")
	}
}

func TestSetBadges(t *testing.T) {
	root := BuildTree([]string{
		"auth/handler.go",
		"auth/token.go",
		"main.go",
	})

	mod := map[string]bool{"auth/handler.go": true}
	SetBadges(root, mod)

	handler := findNode(root, "auth/handler.go")
	if handler == nil {
		t.Fatal("handler not found")
	}
	if handler.Badge != "mod" {
		t.Errorf("handler badge = %q, want %q", handler.Badge, "mod")
	}

	token := findNode(root, "auth/token.go")
	if token == nil {
		t.Fatal("token not found")
	}
	if token.Badge != "" {
		t.Errorf("token badge = %q, want empty", token.Badge)
	}

	main := findNode(root, "main.go")
	if main == nil {
		t.Fatal("main not found")
	}
	if main.Badge != "" {
		t.Errorf("main badge = %q, want empty", main.Badge)
	}
}

func Test_findNode(t *testing.T) {
	root := BuildTree([]string{"a/b/c.go", "d.go"})

	tests := []struct {
		path string
		want bool
	}{
		{"a/b/c.go", true},
		{"d.go", true},
		{"a", true},
		{"a/b", true},
		{"nonexistent", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			n := findNode(root, tt.path)
			if (n != nil) != tt.want {
				t.Errorf("findNode(%q) found=%v, want found=%v", tt.path, n != nil, tt.want)
			}
		})
	}
}

func TestSortOrder_DirsBeforeFiles(t *testing.T) {
	// Files listed before dirs in input — should sort dirs first
	root := BuildTree([]string{
		"zebra.go",
		"alpha/file.go",
		"main.go",
		"beta/file.go",
	})

	names := nodeNames(FlattenVisible(root))
	want := []string{"alpha", "beta", "main.go", "zebra.go"}
	assertNames(t, names, want)
}

func TestExpandToPath_ExpandsAncestors(t *testing.T) {
	root := BuildTree([]string{
		"a/b/c.go",
		"a/b/d.go",
		"a/x.go",
		"top.go",
	})

	// All dirs start collapsed.
	ExpandToPath(root, "a/b/c.go")

	// "a" and "a/b" should now be expanded.
	a := findNode(root, "a")
	if a == nil {
		t.Fatal("dir 'a' not found")
	}
	if a.Collapsed {
		t.Error("dir 'a' should be expanded")
	}

	b := findNode(root, "a/b")
	if b == nil {
		t.Fatal("dir 'a/b' not found")
	}
	if b.Collapsed {
		t.Error("dir 'a/b' should be expanded")
	}

	// Flattening should now show the file.
	flat := FlattenVisible(root)
	names := nodeNames(flat)
	want := []string{"a", "b", "c.go", "d.go", "x.go", "top.go"}
	assertNames(t, names, want)
}

func TestExpandToPath_NonexistentPath(t *testing.T) {
	root := BuildTree([]string{"a/b.go"})
	// Should not panic on missing path.
	ExpandToPath(root, "x/y/z.go")

	a := findNode(root, "a")
	if a == nil {
		t.Fatal("dir 'a' not found")
	}
	if !a.Collapsed {
		t.Error("dir 'a' should remain collapsed")
	}
}

func TestExpandToPath_NilRoot(t *testing.T) {
	// Should not panic.
	ExpandToPath(nil, "a/b.go")
}

func TestExpandToPath_TopLevelFile(t *testing.T) {
	root := BuildTree([]string{"main.go", "a/b.go"})
	// Expanding a top-level file should be a no-op (no dirs to expand).
	ExpandToPath(root, "main.go")

	a := findNode(root, "a")
	if a == nil {
		t.Fatal("dir 'a' not found")
	}
	if !a.Collapsed {
		t.Error("dir 'a' should remain collapsed")
	}
}

// --- helpers ---

func nodeNames(nodes []*TreeNode) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

func assertNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d items %v, want %d items %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("item[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
