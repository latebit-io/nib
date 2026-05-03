package agent

import (
	"strings"
	"testing"
)

func TestPublishProjectMDValidator(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       string
		wantErr    bool
		wantSubstr string
	}{
		{
			name:    "non-project path passes through",
			path:    "/architecture.md",
			body:    "anything goes",
			wantErr: false,
		},
		{
			name:    "valid project.md body passes",
			path:    projectMDPath,
			body:    "# Phase 1: Setup\n## Feature\n- [ ] task\n",
			wantErr: false,
		},
		{
			name:       "invalid project.md body rejected",
			path:       projectMDPath,
			body:       "# Not a phase\n### too deep\n- [ ] orphan\n",
			wantErr:    true,
			wantSubstr: "schema violations",
		},
		{
			name:       "multiple active tasks rejected",
			path:       projectMDPath,
			body:       "# Phase 1: A\n## F\n- [>] one\n- [>] two\n",
			wantErr:    true,
			wantSubstr: "multiple-active-tasks",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := publishProjectMDValidator(tt.path, tt.body)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantSubstr != "" && !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

func TestAppendProjectMDValidator(t *testing.T) {
	t.Run("non-project path passes through", func(t *testing.T) {
		if err := appendProjectMDValidator("/journal.md", "entry"); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
	t.Run("project.md append rejected with task-tool guidance", func(t *testing.T) {
		err := appendProjectMDValidator(projectMDPath, "- [ ] extra")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		// Locks the LLM-facing guidance: the message must steer the
		// model to the structured task tools rather than retrying the
		// raw append. Keep both checks so a future copy edit that
		// drops one tool name still fires here.
		if !strings.Contains(err.Error(), "project_task_add") {
			t.Errorf("error should mention project_task_add: %v", err)
		}
		if !strings.Contains(err.Error(), "update_task") {
			t.Errorf("error should mention update_task: %v", err)
		}
	})
}
