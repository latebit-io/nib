package lint

import (
	"testing"

	enginelint "github.com/latebit-io/nib/engine/lint"
)

func TestFormatFindings(t *testing.T) {
	findings := []enginelint.Finding{
		{Path: "a.go", Line: 3, Col: 7, Linter: "govet", Message: "shadowed var"},
		{Path: "b.go", Line: 12, Message: "unused"},
		{Path: "c.go", Message: "no position"},
	}
	want := "a.go:3:7: [govet] shadowed var\nb.go:12: unused\nc.go: no position"
	if got := FormatFindings(findings); got != want {
		t.Errorf("FormatFindings =\n%q\nwant\n%q", got, want)
	}
	if got := FormatFindings(nil); got != "" {
		t.Errorf("FormatFindings(nil) = %q, want empty", got)
	}
}

func TestGroupPathsByDir(t *testing.T) {
	files, dirs, byDir := GroupPathsByDir([]string{"pkg/a.go", "pkg/b.go", "pkg/a.go", "other/c.go"})
	if len(files) != 3 || files[0] != "pkg/a.go" || files[1] != "pkg/b.go" || files[2] != "other/c.go" {
		t.Errorf("files = %v", files)
	}
	if len(dirs) != 2 || dirs[0] != "pkg" || dirs[1] != "other" {
		t.Errorf("dirs = %v", dirs)
	}
	if len(byDir["pkg"]) != 2 || len(byDir["other"]) != 1 {
		t.Errorf("byDir = %v", byDir)
	}
}
