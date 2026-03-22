package ui

import "testing"

func TestCharsPerTick(t *testing.T) {
	tests := []struct {
		name string
		wpm  int
		want int
	}{
		{"default", 0, 1},                // 800 WPM → ~1 char/tick
		{"high WPM 3000", 3000, 4},       // 3000*5/60*0.016 = 4
		{"low WPM clamps to min", 10, 1}, // clamped to 30 WPM → < 1 → 1
		{"very high WPM 5000", 5000, 6},  // 5000*5/60*0.016 = 6.67 → 6
		{"negative uses default", -1, 1}, // falls through to default
		{"exceeds max clamps", 10000, 6}, // clamped to 5000
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := charsPerTick(tt.wpm)
			if got != tt.want {
				t.Errorf("charsPerTick(%d) = %d, want %d", tt.wpm, got, tt.want)
			}
		})
	}
}
