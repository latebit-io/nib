package lint

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGoVetOutput_findings(t *testing.T) {
	output := `# github.com/latebit-io/junto/engine/agent
engine/agent/agent.go:1510:13: unreachable code
engine/agent/tool.go:42:5: printf format %d has arg x of wrong type string
`
	findings := parseGoVetOutput("", output)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(findings), findings)
	}
	if findings[0].Path != "engine/agent/agent.go" {
		t.Errorf("finding 0 path: %q", findings[0].Path)
	}
	if findings[0].Line != 1510 || findings[0].Col != 13 {
		t.Errorf("finding 0 position: line=%d col=%d", findings[0].Line, findings[0].Col)
	}
	if findings[0].Message != "unreachable code" {
		t.Errorf("finding 0 message: %q", findings[0].Message)
	}
	if findings[1].Path != "engine/agent/tool.go" {
		t.Errorf("finding 1 path: %q", findings[1].Path)
	}
}

func TestParseGoVetOutput_clean(t *testing.T) {
	// Just a package header with no diagnostics.
	if got := parseGoVetOutput("", "# github.com/foo/bar\n"); len(got) != 0 {
		t.Errorf("expected 0 findings, got %+v", got)
	}
	if got := parseGoVetOutput("", ""); len(got) != 0 {
		t.Errorf("empty output must produce 0 findings")
	}
}

func TestParseGoVetOutput_absolutePathNormalization(t *testing.T) {
	root := "/home/dev/project"
	output := "/home/dev/project/engine/agent/agent.go:10:5: problem\n"
	findings := parseGoVetOutput(root, output)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Path != filepath.FromSlash("engine/agent/agent.go") {
		t.Errorf("absolute path not normalized: got %q", findings[0].Path)
	}
}

func TestParseGoVetOutput_dotSlashPrefix(t *testing.T) {
	// go vet commonly emits "./path/file.go:..." for module-relative paths.
	output := "./engine/agent/agent.go:20:1: problem\n"
	findings := parseGoVetOutput("/root", output)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding")
	}
	if findings[0].Path != "engine/agent/agent.go" {
		t.Errorf("./ prefix not stripped: %q", findings[0].Path)
	}
}

func TestParseGoVetOutput_ignoresNonDiagnosticLines(t *testing.T) {
	output := `# pkg
random banner text
vet: ./broken.go: some-level error that is not in the diag format

engine/agent/agent.go:10:5: real finding
Some tail noise`
	findings := parseGoVetOutput("", output)
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding (non-diagnostic lines ignored), got %d: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Message, "real finding") {
		t.Errorf("finding message: %q", findings[0].Message)
	}
}

func TestParseGoVetOutput_longLines(t *testing.T) {
	// Simulate a very long message that would overflow bufio.Scanner's
	// default 64 KiB buffer if not raised.
	long := strings.Repeat("x", 70*1024)
	output := "engine/agent/agent.go:1:1: " + long + "\n"
	findings := parseGoVetOutput("", output)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if len(findings[0].Message) < 70*1024 {
		t.Errorf("long message truncated: got %d bytes", len(findings[0].Message))
	}
}

func TestParseGoVetOutput_tokenLimitExceeded(t *testing.T) {
	// A single "line" that exceeds the 1 MiB buffer cap causes the scanner
	// to stop mid-stream. The parser must not panic, must return whatever
	// findings it did parse before the overflow, and (per the code contract)
	// must log the truncation via slog so operators can diagnose.
	good := "engine/agent/agent.go:1:1: real issue\n"
	overflow := strings.Repeat("x", 2*1024*1024) + "\n"
	findings := parseGoVetOutput("", good+overflow)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding parsed before overflow, got %d", len(findings))
	}
	if findings[0].Line != 1 {
		t.Errorf("pre-overflow finding wrong: %+v", findings[0])
	}
}
