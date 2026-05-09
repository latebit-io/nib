package editor

import (
	"testing"

	"github.com/latebit-io/nib/engine/buffer"
)

//nolint:funlen // table-driven test — length comes from test cases, not complexity
func TestCollapseOverlay(t *testing.T) {
	// Helper to create an editor with given scroll offset and extra lines.
	setup := func(scroll, extra int) *Editor {
		buf := buffer.New()
		// Create 20 lines so scroll offsets are valid.
		buf.Insert(0, 0, "line0\nline1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\nline11\nline12\nline13\nline14\nline15\nline16\nline17\nline18\nline19")
		e := New(buf)
		e.ScrollOffset = scroll
		e.SetExtraVisualLines(extra)
		return e
	}

	// Overlay at lines 5-7 (3 removed lines), 4 added lines.
	// Visual layout:
	//   0-4: normal
	//   5-7: removed (3 lines)
	//   8-11: added (4 virtual lines)
	//   12+: normal (buffer line 8+)

	tests := []struct {
		name           string
		scroll         int
		startLine      int
		endLine        int
		addedCount     int
		bufferMutated  bool
		wantScroll     int
		wantExtraLines int
	}{
		// --- Reject path (buffer unchanged) ---
		{
			name:       "reject: scroll before overlay",
			scroll:     2,
			startLine:  5,
			endLine:    7,
			addedCount: 4,
			wantScroll: 2,
		},
		{
			name:       "reject: scroll past overlay",
			scroll:     15,
			startLine:  5,
			endLine:    7,
			addedCount: 4,
			wantScroll: 11, // 15 - 4 = 11
		},
		{
			name:       "reject: scroll in added zone",
			scroll:     9,
			startLine:  5,
			endLine:    7,
			addedCount: 4,
			wantScroll: 8, // endLine + 1
		},
		{
			name:       "reject: scroll on endLine boundary",
			scroll:     7,
			startLine:  5,
			endLine:    7,
			addedCount: 4,
			wantScroll: 7, // at endLine, not > endLine
		},

		// --- Approve path (buffer mutated) ---
		{
			name:          "approve: scroll before overlay",
			scroll:        2,
			startLine:     5,
			endLine:       7,
			addedCount:    4,
			bufferMutated: true,
			wantScroll:    2,
		},
		{
			name:          "approve: scroll past overlay",
			scroll:        15,
			startLine:     5,
			endLine:       7,
			addedCount:    4,
			bufferMutated: true,
			wantScroll:    12, // 15 - removedCount(3) = 12
		},
		{
			name:          "approve: scroll in removed range",
			scroll:        6,
			startLine:     5,
			endLine:       7,
			addedCount:    4,
			bufferMutated: true,
			wantScroll:    5, // clamped to startLine
		},
		{
			name:          "approve: scroll in added zone",
			scroll:        9,
			startLine:     5,
			endLine:       7,
			addedCount:    4,
			bufferMutated: true,
			wantScroll:    6, // startLine + (9 - 7 - 1) = 5 + 1 = 6
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := setup(tt.scroll, tt.addedCount)
			e.CollapseOverlay(tt.startLine, tt.endLine, tt.addedCount, tt.bufferMutated)
			if e.ScrollOffset != tt.wantScroll {
				t.Errorf("ScrollOffset = %d, want %d", e.ScrollOffset, tt.wantScroll)
			}
			if e.ExtraVisualLines() != 0 {
				t.Errorf("ExtraVisualLines = %d, want 0", e.ExtraVisualLines())
			}
		})
	}
}
