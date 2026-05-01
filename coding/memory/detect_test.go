package memory

import (
	"slices"
	"testing"
)

func TestDetectDistributedMemory(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"team-demarkus", true},
		{"shared-docs", true},
		{"distributed-wiki", true},
		{"demarkus-soul", true},
		{"Team-Server", true},  // case-insensitive
		{"my-SHARED-db", true}, // keyword anywhere
		{"project-tools", false},
		{"memory", false},
		{"lsp-server", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectDistributedMemory([]string{tt.name})
			got := len(result) > 0
			if got != tt.want {
				t.Errorf("DetectDistributedMemory([%q]) returned %v, want match=%v", tt.name, result, tt.want)
			}
		})
	}
}

func TestDetectDistributedMemoryFiltersCorrectly(t *testing.T) {
	input := []string{"team-wiki", "lsp-server", "shared-docs", "my-linter"}
	got := DetectDistributedMemory(input)
	want := []string{"team-wiki", "shared-docs"}
	if !slices.Equal(got, want) {
		t.Fatalf("DetectDistributedMemory(%v) = %v, want %v", input, got, want)
	}
}
