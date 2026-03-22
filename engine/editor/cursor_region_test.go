package editor

import "testing"

func TestCursorInRegion(t *testing.T) {
	tests := []struct {
		name                                 string
		cursorLine, cursorCol                int
		startLine, startCol, endLine, endCol int
		want                                 bool
	}{
		// Single-line region
		{"inside single-line", 5, 10, 5, 5, 5, 15, true},
		{"at start of single-line", 5, 5, 5, 5, 5, 15, true},
		{"at end of single-line", 5, 15, 5, 5, 5, 15, true},
		{"before single-line", 5, 4, 5, 5, 5, 15, false},
		{"after single-line", 5, 16, 5, 5, 5, 15, false},
		{"wrong line single-line", 4, 10, 5, 5, 5, 15, false},

		// Multi-line region
		{"first line inside", 2, 10, 2, 5, 4, 8, true},
		{"first line at start col", 2, 5, 2, 5, 4, 8, true},
		{"first line before start col", 2, 4, 2, 5, 4, 8, false},
		{"interior line", 3, 0, 2, 5, 4, 8, true},
		{"interior line any col", 3, 99, 2, 5, 4, 8, true},
		{"last line inside", 4, 5, 2, 5, 4, 8, true},
		{"last line at end col", 4, 8, 2, 5, 4, 8, true},
		{"last line past end col", 4, 9, 2, 5, 4, 8, false},
		{"above region", 1, 10, 2, 5, 4, 8, false},
		{"below region", 5, 0, 2, 5, 4, 8, false},

		// Zero-width region (cursor at a point)
		{"exact match zero-width", 3, 7, 3, 7, 3, 7, true},
		{"off by one zero-width", 3, 8, 3, 7, 3, 7, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CursorInRegion(
				tt.cursorLine, tt.cursorCol,
				tt.startLine, tt.startCol,
				tt.endLine, tt.endCol,
			)
			if got != tt.want {
				t.Errorf("CursorInRegion(%d,%d, %d,%d→%d,%d) = %v, want %v",
					tt.cursorLine, tt.cursorCol,
					tt.startLine, tt.startCol, tt.endLine, tt.endCol,
					got, tt.want)
			}
		})
	}
}
