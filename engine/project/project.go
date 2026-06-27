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
//	# Phase 1: Foundation
//	## Feature
//	- [ ] pending goal
//	- [x] completed goal
//	- [>] active goal
//
// Heading depth maps to tree depth: h1 (depth 0) is a phase, h2
// (depth 1) is a feature. Task-list items are leaf nodes directly
// beneath a feature. The strict schema (h1 phases matching
// "Phase N: Title", h2 features, tasks under features, at most one
// active task) is enforced by [Validate]; the parser itself is lenient
// so a malformed document still loads for diagnosis.
package project

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// phaseNumberPattern captures the leading number N from a
// "Phase N: Title" heading. Used by [Tree.nextPhaseNumber] to compute
// the number for an appended phase.
var phaseNumberPattern = regexp.MustCompile(`^Phase\s+(\d+):`)

// FormatPhaseHeading returns a schema-valid phase heading title. A
// title already in the canonical "Phase N: Title" form is returned
// trimmed and verbatim — the caller's explicit numbering wins —
// otherwise the descriptive title is prefixed as "Phase <num>: <title>".
// Used by both [Tree.AddPhase] and the session's project_init skeleton
// builder so every write path produces a heading that [Validate] accepts.
func FormatPhaseHeading(num int, title string) string {
	title = strings.TrimSpace(title)
	if phaseHeadingPattern.MatchString(title) {
		return title
	}
	return fmt.Sprintf("Phase %d: %s", num, title)
}

// StripPhaseNumber returns a phase heading's descriptive body with any
// leading "Phase N:" prefix removed and surrounding whitespace trimmed.
// A title without the prefix is returned trimmed but otherwise unchanged.
// Callers use it to compare phases by descriptive identity — "Phase 9:
// Polish" and a bare "Polish" denote the same phase, so a duplicate guard
// keyed on this value catches mixed bare/numbered forms.
func StripPhaseNumber(title string) string {
	stripped := phaseNumberPattern.ReplaceAllString(strings.TrimSpace(title), "")
	return strings.TrimSpace(stripped)
}

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

// FindNextPendingTask returns the title of the first leaf task in
// document order whose status is TaskPending. Used by the agent's
// auto-continue path to suggest the next task to activate after one
// completes — without an explicit hint, the LLM tends to stop and
// wait for developer guidance even under autonomy levels that allow
// continuation.
//
// Returns "" when no pending task remains. Callers treat that as
// "all done" and refrain from injecting a continuation prompt.
func (t *Tree) FindNextPendingTask() string {
	for _, root := range t.Roots {
		if title := findFirstPending(root); title != "" {
			return title
		}
	}
	return ""
}

// findFirstPending walks n depth-first and returns the title of the
// first leaf task with TaskPending status. Empty string when no
// pending task exists in the subtree.
func findFirstPending(n *Node) string {
	if !n.IsHeading && n.Status == TaskPending {
		return n.Title
	}
	for _, child := range n.Children {
		if title := findFirstPending(child); title != "" {
			return title
		}
	}
	return ""
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

// AddTask appends a new pending task under the given phase and feature.
//   - phase: case-insensitive substring matched against h1 titles
//     (e.g. "Phase 3" or "rendering"); must match exactly one phase.
//   - feature: case-insensitive exact match against h2 titles under the
//     phase. Created as a new h2 if no match exists.
//   - task: the task title (imperative, what changes).
//   - link: optional memory-doc path; when non-empty, appended to the
//     task title as a markdown link: "Title ([details](link))".
//
// Returns an error if the phase cannot be located OR if a task with the
// same body already exists anywhere in the tree. Tree-scoped dedup is
// load-bearing: the activate/complete tools resolve by title across
// the whole tree, so two same-titled tasks make those tools ambiguous
// and one ends up orphaned. Comparison is case-insensitive and ignores
// the " ([details](...))" link suffix so two attempts at the same task
// — one with a link, one without — still collide.
func (t *Tree) AddTask(phase, feature, task, link string) error {
	phaseNode := findPhaseBySubstring(t, phase)
	if phaseNode == nil {
		return fmt.Errorf("no phase matching %q", phase)
	}
	if existing := findTaskByBody(t, task); existing != nil {
		return fmt.Errorf("task with body %q already exists (current title: %q)", strings.TrimSpace(task), existing.Title)
	}
	featureNode := findFeatureByTitle(phaseNode, feature)
	if featureNode == nil {
		featureNode = &Node{
			Title:     strings.TrimSpace(feature),
			Depth:     1,
			IsHeading: true,
		}
		phaseNode.Children = append(phaseNode.Children, featureNode)
	}
	title := strings.TrimSpace(task)
	if l := strings.TrimSpace(link); l != "" {
		title = fmt.Sprintf("%s ([details](%s))", title, l)
	}
	featureNode.Children = append(featureNode.Children, &Node{
		Title:     title,
		Depth:     2,
		IsHeading: false,
		Status:    TaskPending,
	})
	return nil
}

// AddPhase appends a new top-level phase (h1) heading to the tree and
// returns its full, schema-valid title. A bare descriptive title
// ("Polish") is auto-numbered as "Phase N: Polish" where N is one
// greater than the highest existing phase number; a title already in
// the "Phase N: Title" form is used verbatim (see [FormatPhaseHeading]).
// Phases always append at the end of the document.
//
// This fills the gap between project_init (which seeds phases on a
// fresh repo but is idempotent — it refuses to touch an existing plan)
// and AddTask (which creates features under an existing phase, never a
// new phase).
//
// Returns an error if the trimmed title is empty or a phase with the
// same full title already exists (case-insensitive). Unlike tasks,
// phases are not de-duplicated by descriptive body — two phases that
// share a suffix under different numbers ("Phase 2: Polish",
// "Phase 5: Polish") are legitimate, so only an exact full-title
// collision is rejected.
func (t *Tree) AddPhase(title string) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", fmt.Errorf("phase title is empty")
	}
	full := FormatPhaseHeading(t.nextPhaseNumber(), title)
	for _, root := range t.Roots {
		if strings.EqualFold(strings.TrimSpace(root.Title), full) {
			return "", fmt.Errorf("phase %q already exists", full)
		}
	}
	t.Roots = append(t.Roots, &Node{
		Title:     full,
		Depth:     0,
		IsHeading: true,
	})
	return full, nil
}

// nextPhaseNumber returns the number to assign the next appended phase:
// one greater than the highest "Phase N" number present, or one greater
// than the root count when no root carries a parseable number (e.g. a
// tree seeded by an older project_init that wrote bare h1 titles).
// Falling back to the root count rather than 1 avoids handing the same
// number to successive appends against a tree of legacy bare phases.
func (t *Tree) nextPhaseNumber() int {
	maxN := len(t.Roots)
	for _, root := range t.Roots {
		m := phaseNumberPattern.FindStringSubmatch(root.Title)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > maxN {
			maxN = n
		}
	}
	return maxN + 1
}

// MarkDone marks the task at targetTitle as done (TaskDone).
// If the task was the active goal, the active marker is simply replaced
// with done — no new active goal is selected automatically.
// Returns true if the target was found and updated.
// Only leaf tasks (IsHeading=false) can be marked done.
func (t *Tree) MarkDone(targetTitle string) bool {
	for _, root := range t.Roots {
		if n := findTaskByTitle(root, targetTitle); n != nil {
			n.Status = TaskDone
			return true
		}
	}
	return false
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

// findPhaseBySubstring returns the first root (h1) heading whose title
// contains the given substring (case-insensitive). Returns nil if no
// phase matches. Used by AddTask to locate the insertion point.
func findPhaseBySubstring(t *Tree, search string) *Node {
	needle := strings.ToLower(strings.TrimSpace(search))
	if needle == "" {
		return nil
	}
	for _, root := range t.Roots {
		if root.IsHeading && root.Depth == 0 &&
			strings.Contains(strings.ToLower(root.Title), needle) {
			return root
		}
	}
	return nil
}

// findFeatureByTitle returns the h2 child of phase whose title matches
// (case-insensitive, trimmed) or nil if none exists.
func findFeatureByTitle(phase *Node, title string) *Node {
	want := strings.ToLower(strings.TrimSpace(title))
	for _, child := range phase.Children {
		if child.IsHeading && strings.ToLower(child.Title) == want {
			return child
		}
	}
	return nil
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

// findTaskByBody returns the first leaf task whose body matches the
// given task text, case-insensitively, with the optional
// " ([details](...))" link suffix stripped from the stored title
// before comparing. Used by [Tree.AddTask] for tree-scoped duplicate
// detection — the activate/complete tools resolve by title across the
// whole tree, so the work tree must have at most one task per body.
func findTaskByBody(t *Tree, task string) *Node {
	target := strings.ToLower(strings.TrimSpace(task))
	if target == "" {
		return nil
	}
	var found *Node
	t.Walk(func(n *Node) bool {
		if found != nil {
			return false
		}
		if n.IsHeading {
			return true
		}
		if strings.ToLower(strings.TrimSpace(stripDetailsSuffix(n.Title))) == target {
			found = n
			return false
		}
		return true
	})
	return found
}

// stripDetailsSuffix removes the optional " ([details](path))" tail
// that [Tree.AddTask] appends when a link is supplied. Pure suffix
// strip — titles without the marker are returned unchanged. Defensive
// HasSuffix check so a degenerate input ending in just "))" isn't
// misread as having the marker.
func stripDetailsSuffix(title string) string {
	const opener = " ([details]("
	if !strings.HasSuffix(title, "))") {
		return title
	}
	idx := strings.LastIndex(title, opener)
	if idx == -1 {
		return title
	}
	return title[:idx]
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
