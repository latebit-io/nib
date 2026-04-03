package glob

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// Exact
		{"foo.txt", "foo.txt", true},
		{"foo.txt", "bar.txt", false},

		// Star
		{"*.log", "error.log", true},
		{"*.log", "error.txt", false},
		{"*.log", "dir/error.log", false},

		// Double star
		{"**/*.log", "error.log", true},
		{"**/*.log", "dir/error.log", true},
		{"**/*.log", "a/b/c/error.log", true},
		{"**/foo", "foo", true},
		{"**/foo", "dir/foo", true},
		{"**/foo", "barfoo", false},

		// Question mark
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},

		// Path patterns
		{"src/main.go", "src/main.go", true},
		{"src/main.go", "other/main.go", false},

		// Mixed
		{"engine/*/buffer.go", "engine/buffer/buffer.go", true},
		{"engine/*/buffer.go", "engine/a/b/buffer.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.name, func(t *testing.T) {
			got := Match(tt.pattern, tt.name)
			if got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v",
					tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}
