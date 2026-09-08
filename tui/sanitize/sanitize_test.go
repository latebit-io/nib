package sanitize

import "testing"

func TestSanitize(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"plain passthrough", []string{"hello, wörld"}, "hello, wörld"},
		{"newline and tab kept", []string{"a\n\tb"}, "a\n\tb"},
		{"other control chars dropped", []string{"a\x00b\rc\x7fd"}, "abcd"},
		{"CSI color", []string{"\x1b[31mred\x1b[0m"}, "red"},
		{"CSI multi-param", []string{"\x1b[1;32;40mx\x1b[K"}, "x"},
		{"lone ESC dropped", []string{"a\x1bb"}, "ab"},
		{"ESC at end then plain", []string{"a\x1b", "b"}, "ab"},
		{"ESC split before bracket", []string{"a\x1b", "[31mb"}, "ab"},
		{"CSI params split", []string{"a\x1b[3", "1mb"}, "ab"},
		{"CSI final byte split", []string{"a\x1b[31", "mb"}, "ab"},
		{"OSC hyperlink BEL-terminated", []string{"\x1b]8;;http://x\ay\x1b]8;;\a"}, "y"},
		{"OSC title ST-terminated", []string{"\x1b]0;title\x1b\\z"}, "z"},
		{"OSC body split", []string{"a\x1b]0;ti", "tle\ab"}, "ab"},
		{"OSC ST split at ESC", []string{"a\x1b]0;t\x1b", "\\b"}, "ab"},
		{"OSC intro split", []string{"a\x1b", "]0;t\ab"}, "ab"},
		{"double ESC then CSI", []string{"\x1b\x1b[31mq"}, "q"},
		{"empty chunk keeps state", []string{"\x1b[3", "", "1mk"}, "k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var z Sanitizer
			var got string
			for _, c := range tt.chunks {
				got += z.Sanitize(c)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if z.inCSI || z.inOSC || z.pendingESC {
				t.Errorf("state not reset: %+v", z)
			}
		})
	}
}
