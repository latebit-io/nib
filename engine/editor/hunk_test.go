package editor

import "testing"

//nolint:funlen // table-driven test — length comes from test cases, not complexity
func assertHunkEqual(t *testing.T, idx int, got, want Hunk) {
	t.Helper()
	if got.Op != want.Op {
		t.Errorf("hunk[%d].Op = %d, want %d", idx, got.Op, want.Op)
	}
	if got.SearchStart != want.SearchStart {
		t.Errorf("hunk[%d].SearchStart = %d, want %d", idx, got.SearchStart, want.SearchStart)
	}
	if got.SearchCount != want.SearchCount {
		t.Errorf("hunk[%d].SearchCount = %d, want %d", idx, got.SearchCount, want.SearchCount)
	}
	if len(got.ReplaceLines) != len(want.ReplaceLines) {
		t.Errorf("hunk[%d].ReplaceLines = %v, want %v", idx, got.ReplaceLines, want.ReplaceLines)
		return
	}
	for j := range got.ReplaceLines {
		if got.ReplaceLines[j] != want.ReplaceLines[j] {
			t.Errorf("hunk[%d].ReplaceLines[%d] = %q, want %q", idx, j, got.ReplaceLines[j], want.ReplaceLines[j])
		}
	}
}

//nolint:funlen // table-driven test — length comes from test cases, not complexity
func TestComputeHunks(t *testing.T) {
	tests := []struct {
		name    string
		search  []string
		replace []string
		want    []Hunk
	}{
		{
			name:    "identical lines",
			search:  []string{"a", "b", "c"},
			replace: []string{"a", "b", "c"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 3},
			},
		},
		{
			name:    "all lines different",
			search:  []string{"a", "b"},
			replace: []string{"x", "y"},
			want: []Hunk{
				{Op: HunkModify, SearchStart: 0, SearchCount: 1, ReplaceLines: []string{"x"}},
				{Op: HunkModify, SearchStart: 1, SearchCount: 1, ReplaceLines: []string{"y"}},
			},
		},
		{
			name:    "middle line changed",
			search:  []string{"a", "b", "c"},
			replace: []string{"a", "X", "c"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 1},
				{Op: HunkModify, SearchStart: 1, SearchCount: 1, ReplaceLines: []string{"X"}},
				{Op: HunkKeep, SearchStart: 2, SearchCount: 1},
			},
		},
		{
			name:    "line deleted in middle",
			search:  []string{"a", "b", "c", "d"},
			replace: []string{"a", "d"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 1},
				{Op: HunkDelete, SearchStart: 1, SearchCount: 2},
				{Op: HunkKeep, SearchStart: 3, SearchCount: 1},
			},
		},
		{
			name:    "line inserted in middle",
			search:  []string{"a", "d"},
			replace: []string{"a", "b", "c", "d"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 1},
				{Op: HunkInsert, SearchStart: 1, SearchCount: 0, ReplaceLines: []string{"b", "c"}},
				{Op: HunkKeep, SearchStart: 1, SearchCount: 1},
			},
		},
		{
			name:    "pure deletion",
			search:  []string{"a", "b", "c"},
			replace: []string{},
			want: []Hunk{
				{Op: HunkDelete, SearchStart: 0, SearchCount: 3},
			},
		},
		{
			name:    "pure insertion",
			search:  []string{},
			replace: []string{"a", "b"},
			want: []Hunk{
				{Op: HunkInsert, SearchStart: 0, SearchCount: 0, ReplaceLines: []string{"a", "b"}},
			},
		},
		{
			name:    "both empty",
			search:  []string{},
			replace: []string{},
			want:    nil,
		},
		{
			name:    "single line unchanged",
			search:  []string{"a"},
			replace: []string{"a"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 1},
			},
		},
		{
			name:    "single line modified",
			search:  []string{"a"},
			replace: []string{"b"},
			want: []Hunk{
				{Op: HunkModify, SearchStart: 0, SearchCount: 1, ReplaceLines: []string{"b"}},
			},
		},
		{
			name:    "modify and insert",
			search:  []string{"a", "b", "c"},
			replace: []string{"a", "X", "Y", "Z", "c"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 1},
				{Op: HunkModify, SearchStart: 1, SearchCount: 1, ReplaceLines: []string{"X"}},
				{Op: HunkInsert, SearchStart: 2, SearchCount: 0, ReplaceLines: []string{"Y", "Z"}},
				{Op: HunkKeep, SearchStart: 2, SearchCount: 1},
			},
		},
		{
			name:    "modify and delete",
			search:  []string{"a", "b", "c", "d", "e"},
			replace: []string{"a", "X", "e"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 1},
				{Op: HunkModify, SearchStart: 1, SearchCount: 1, ReplaceLines: []string{"X"}},
				{Op: HunkDelete, SearchStart: 2, SearchCount: 2},
				{Op: HunkKeep, SearchStart: 4, SearchCount: 1},
			},
		},
		{
			name:    "prefix only change",
			search:  []string{"a", "b", "c"},
			replace: []string{"X", "b", "c"},
			want: []Hunk{
				{Op: HunkModify, SearchStart: 0, SearchCount: 1, ReplaceLines: []string{"X"}},
				{Op: HunkKeep, SearchStart: 1, SearchCount: 2},
			},
		},
		{
			name:    "suffix only change",
			search:  []string{"a", "b", "c"},
			replace: []string{"a", "b", "X"},
			want: []Hunk{
				{Op: HunkKeep, SearchStart: 0, SearchCount: 2},
				{Op: HunkModify, SearchStart: 2, SearchCount: 1, ReplaceLines: []string{"X"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeHunks(tt.search, tt.replace)
			if len(got) != len(tt.want) {
				t.Fatalf("len(hunks) = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				assertHunkEqual(t, i, got[i], tt.want[i])
			}
		})
	}
}
