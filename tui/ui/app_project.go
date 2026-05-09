package ui

import (
	"log/slog"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

// Project-pane file-system message handlers.
// Extracted from app.go's Update() (Phase 7 of AppModel decomposition).
//
// All session writes go through AppModel — the project pane component
// emits intent messages (ProjectCreate*, ProjectDelete*, etc.) and
// AppModel mediates the actual session call. This is the bridge layer.
//
// Goal-state handlers (ProjectSetActiveGoalMsg, ProjectMarkGoalDoneMsg)
// live in `app_work_tree.go` — they're work-tree concerns, not file-
// system concerns. `reloadWorkTreeResultMsg` and `reloadWorkTreeCmd`
// also live there.

// handleProjectOpenFile opens the file selected in the project pane.
func (m *AppModel) handleProjectOpenFile(msg ProjectOpenFileMsg) (tea.Model, tea.Cmd) {
	return m.openFile(msg.Path)
}

// handleProjectAddContext adds a path to the session's context set and
// refreshes the project pane to render the new badge.
func (m *AppModel) handleProjectAddContext(msg ProjectAddContextMsg) (tea.Model, tea.Cmd) {
	m.Session.AddContext(msg.Path)
	m.refreshProjectPane()
	return m, nil
}

// handleProjectRemoveContext removes a path from the session's context set
// and refreshes the project pane.
func (m *AppModel) handleProjectRemoveContext(msg ProjectRemoveContextMsg) (tea.Model, tea.Cmd) {
	m.Session.RemoveContext(msg.Path)
	m.refreshProjectPane()
	return m, nil
}

// handleProjectCreateFile creates an empty file at the given path and
// opens it in the editor on success. Errors are surfaced in the agent
// pane; the pane refresh runs only after a successful create so a failed
// create doesn't shuffle the tree.
func (m *AppModel) handleProjectCreateFile(msg ProjectCreateFileMsg) (tea.Model, tea.Cmd) {
	if err := m.Session.WriteFile(msg.Path, ""); err != nil {
		slog.Warn("create file", "err", err)
		m.AgentPane.AppendMeta("[create failed: " + err.Error() + "]\n")
		return m, nil
	}
	m.refreshProjectPane()
	absPath := filepath.Join(m.Session.ProjectRoot(), msg.Path)
	return m.openFile(absPath)
}

// handleProjectCreateDir creates a new directory and synthesizes an
// empty-dir entry in the tree (real dirs only show up after Walk on
// next refresh; AddEmptyDir surfaces it immediately for navigation).
func (m *AppModel) handleProjectCreateDir(msg ProjectCreateDirMsg) (tea.Model, tea.Cmd) {
	if err := m.Session.CreateDir(msg.Path); err != nil {
		slog.Warn("create dir", "err", err)
		m.AgentPane.AppendMeta("[create dir failed: " + err.Error() + "]\n")
		return m, nil
	}
	m.ProjectPane.AddEmptyDir(msg.Path)
	m.refreshProjectPane()
	return m, nil
}

// handleToggleProject implements the Ctrl+B toggle:
// hidden → show + focus, visible but not focused → focus, focused → hide.
func (m *AppModel) handleToggleProject() (tea.Model, tea.Cmd) {
	r := m.Regions.regionByName("project")
	if r == nil {
		return m, nil
	}
	focused := m.Regions.FocusedRegion()
	if !r.Visible {
		// Hidden → show + focus (rebuild if stale)
		if m.ProjectPane.dirty {
			m.ProjectPane.rebuild()
			m.ProjectPane.dirty = false
		}
		m.Regions.Show("project")
		m.Regions.FocusByName("project")
	} else if focused == nil || focused.Name != "project" {
		// Visible but not focused → focus
		m.Regions.FocusByName("project")
	} else {
		// Focused → hide, move focus to editor
		m.Regions.Hide("project")
		m.Regions.FocusByName("editor")
	}
	return m, nil
}

// handleProjectDeleteFile removes a file (or directory) from disk + the
// session's bookkeeping. Cleanup discipline:
//
//   - Drop any pooled editors keyed by the deleted path or its children
//     (when a directory is removed) so the editor pool doesn't leak
//     tree-sitter highlighter instances.
//   - Preserve the parent directory in the project tree if it became
//     empty on disk after the delete (otherwise it would silently
//     disappear since it has no children to render).
//   - Rebuild the editor pane only when the active file actually
//     changed (deleted file or its containing directory) — the session
//     falls back to another open file in that case, and the editor
//     model needs to follow.
func (m *AppModel) handleProjectDeleteFile(msg ProjectDeleteFileMsg) (tea.Model, tea.Cmd) {
	absPath := filepath.Join(m.Session.ProjectRoot(), msg.Path)
	prevActive := m.Session.ActiveFile()
	if err := m.Session.DeleteFile(absPath); err != nil {
		slog.Warn("delete file", "err", err)
		m.AgentPane.AppendMeta("[delete failed: " + err.Error() + "]\n")
		return m, nil
	}
	m.dropPooledEditors(absPath)
	parentRel := filepath.ToSlash(filepath.Dir(msg.Path))
	if parentRel != "." && parentRel != "" {
		parentAbs := filepath.Join(m.Session.ProjectRoot(), parentRel)
		if entries, err := os.ReadDir(parentAbs); err == nil && len(entries) == 0 {
			m.ProjectPane.AddEmptyDir(parentRel)
		}
	}
	m.refreshProjectPane()
	if m.Session.ActiveFile() != prevActive {
		m.rebuildEditorModel()
	}
	return m, nil
}
