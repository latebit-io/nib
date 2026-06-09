package lang

import "testing"

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"go.mod", "go.mod"},
		{"/path/to/file.ts", "typescript"},
		{"component.tsx", "typescriptreact"},
		{"script.py", "python"},
		{"lib.rs", "rust"},
		{"main.c", "c"},
		{"header.h", "c"},
		{"app.cpp", "cpp"},
		{"page.html", "html"},
		{"style.css", "css"},
		{"config.json", "json"},
		{"deploy.yaml", "yaml"},
		{"README.md", "markdown"},
		{"query.sql", "sql"},
		{"main.zig", "zig"},
		{"server.ex", "elixir"},
		{"main.dart", "dart"},
		{"infra.tf", "terraform"},
		{"script.sh", "shellscript"},
		// Unknown extension
		{"Makefile", ""},
		{"", ""},
		{".gitignore", ""},
		// Case insensitive
		{"FILE.GO", "go"},
		{"Main.Py", "python"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := DetectLanguage(tt.path)
			if got != tt.want {
				t.Errorf("DetectLanguage(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
