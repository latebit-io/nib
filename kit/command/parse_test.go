package command

import "testing"

func TestParseSlash(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantName string
		wantArgs string
		wantOk   bool
	}{
		{name: "valid simple", input: "/foo", wantName: "foo", wantArgs: "", wantOk: true},
		{name: "valid with args", input: "/foo bar baz", wantName: "foo", wantArgs: "bar baz", wantOk: true},
		{name: "multiple spaces collapse", input: "/foo   bar", wantName: "foo", wantArgs: "bar", wantOk: true},
		{name: "trailing space empty args", input: "/foo ", wantName: "foo", wantArgs: "", wantOk: true},
		{name: "trailing spaces empty args", input: "/foo    ", wantName: "foo", wantArgs: "", wantOk: true},
		{name: "uppercase normalized", input: "/FOO", wantName: "foo", wantArgs: "", wantOk: true},
		{name: "mixed case normalized", input: "/FooBar", wantName: "foobar", wantArgs: "", wantOk: true},
		{name: "name with hyphens", input: "/foo-bar", wantName: "foo-bar", wantArgs: "", wantOk: true},
		{name: "name with underscores", input: "/foo_bar", wantName: "foo_bar", wantArgs: "", wantOk: true},
		{name: "name with digits", input: "/foo123", wantName: "foo123", wantArgs: "", wantOk: true},
		{name: "args preserved verbatim", input: "/foo a b!c d/e", wantName: "foo", wantArgs: "a b!c d/e", wantOk: true},

		{name: "empty", input: "", wantOk: false},
		{name: "just slash", input: "/", wantOk: false},
		{name: "no slash prefix", input: "foo bar", wantOk: false},
		{name: "invalid char bang", input: "/foo!", wantOk: false},
		{name: "invalid char dot", input: "/foo.bar", wantOk: false},
		{name: "newline in input", input: "/foo\nbar", wantOk: false},
		{name: "newline before name", input: "\n/foo", wantOk: false},
		{name: "carriage return", input: "/foo\rbar", wantOk: false},
		{name: "leading whitespace", input: " /foo", wantOk: false},
		{name: "tab in name", input: "/foo\tbar", wantOk: false},
		{name: "empty name with args", input: "/ foo", wantOk: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotArgs, gotOk := parseSlash(tc.input)
			if gotOk != tc.wantOk {
				t.Fatalf("ok = %v, want %v (got name=%q args=%q)", gotOk, tc.wantOk, gotName, gotArgs)
			}
			if !tc.wantOk {
				return
			}
			if gotName != tc.wantName {
				t.Errorf("name = %q, want %q", gotName, tc.wantName)
			}
			if gotArgs != tc.wantArgs {
				t.Errorf("args = %q, want %q", gotArgs, tc.wantArgs)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"foo", true},
		{"foo123", true},
		{"foo-bar", true},
		{"foo_bar", true},
		{"FOO", true},
		{"a", true},
		{"", false},
		{"foo!", false},
		{"foo.bar", false},
		{"foo bar", false},
		{"foo/bar", false},
	}
	for _, tc := range cases {
		if got := ValidName(tc.in); got != tc.want {
			t.Errorf("ValidName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
