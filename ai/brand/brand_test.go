package brand

import "testing"

func TestBashApprovalEnabled(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		defaultOn bool
		want      bool
	}{
		{"unset defers to default on", "", true, true},
		{"unset defers to default off", "", false, false},
		{"zero kills default-on", "0", true, false},
		{"false kills default-on", "false", true, false},
		{"off kills default-on", "off", true, false},
		{"case-insensitive kill", "OFF", true, false},
		{"one arms default-off", "1", false, true},
		{"arbitrary value arms", "yes", false, true},
		{"whitespace-only is unset", "   ", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvKeyBashApproval, c.env)
			if got := BashApprovalEnabled(c.defaultOn); got != c.want {
				t.Fatalf("BashApprovalEnabled(%v) with env %q = %v, want %v", c.defaultOn, c.env, got, c.want)
			}
		})
	}
}
