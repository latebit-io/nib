package lsp

import "testing"

func TestPathToURI(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/Users/test/main.go", "file:///Users/test/main.go"},
		{"/path/with spaces/file.go", "file:///path/with%20spaces/file.go"},
	}
	for _, tt := range tests {
		got := pathToURI(tt.path)
		if got != tt.want {
			t.Errorf("pathToURI(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestURIToPath(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{"file:///Users/test/main.go", "/Users/test/main.go"},
		{"file:///path/with%20spaces/file.go", "/path/with spaces/file.go"},
		{"/just/a/path", "/just/a/path"}, // no file:// prefix
	}
	for _, tt := range tests {
		got := uriToPath(tt.uri)
		if got != tt.want {
			t.Errorf("uriToPath(%q) = %q, want %q", tt.uri, got, tt.want)
		}
	}
}

func TestURIRoundTrip(t *testing.T) {
	paths := []string{
		"/Users/test/main.go",
		"/tmp/junto/file.ts",
	}
	for _, path := range paths {
		got := uriToPath(pathToURI(path))
		if got != path {
			t.Errorf("round-trip %q: got %q", path, got)
		}
	}
}
