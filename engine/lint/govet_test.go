package lint

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// captureSlog swaps the default slog logger for a text handler writing to
// a buffer for the duration of the test. Returns the buffer so callers can
// assert on emitted records. The default logger is restored on cleanup.
func captureSlog(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

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
	// findings it did parse before the overflow, and must log the truncation
	// via slog — otherwise trailing diagnostics are silently dropped, which
	// violates the "no silently swallowed errors" rule.
	logs := captureSlog(t, slog.LevelWarn)

	good := "engine/agent/agent.go:1:1: real issue\n"
	overflow := strings.Repeat("x", 2*1024*1024) + "\n"
	findings := parseGoVetOutput("", good+overflow)

	if len(findings) != 1 {
		t.Fatalf("expected 1 finding parsed before overflow, got %d", len(findings))
	}
	if findings[0].Line != 1 {
		t.Errorf("pre-overflow finding wrong: %+v", findings[0])
	}

	logged := logs.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("expected WARN-level log on scanner truncation, got: %q", logged)
	}
	if !strings.Contains(logged, "truncated") {
		t.Errorf("log should mention truncation (contract), got: %q", logged)
	}
	// Sanity-check the structured attributes — losing them would hurt
	// debuggability in production.
	if !strings.Contains(logged, "parsed_findings=1") {
		t.Errorf("log should record count of findings parsed before overflow, got: %q", logged)
	}
}

func TestParseGoVetOutput_overflowPositionSkipped(t *testing.T) {
	// A line/column larger than MaxInt must not produce a zero-position
	// finding — that would send the agent to line 0 of a real file. Skip
	// the diagnostic and log so operators know the output was malformed.
	logs := captureSlog(t, slog.LevelWarn)

	// Construct positions that overflow int64 on any platform. strconv.Atoi
	// returns ErrRange for these.
	overflow := "99999999999999999999"
	output := "engine/agent/agent.go:" + overflow + ":1: bad line\n" +
		"engine/agent/agent.go:1:" + overflow + ": bad col\n" +
		"engine/agent/agent.go:10:5: valid finding\n"

	findings := parseGoVetOutput("", output)
	if len(findings) != 1 {
		t.Fatalf("expected only the valid finding to survive, got %d: %+v", len(findings), findings)
	}
	if findings[0].Line != 10 || findings[0].Col != 5 {
		t.Errorf("wrong finding survived: %+v", findings[0])
	}

	logged := logs.String()
	if !strings.Contains(logged, "line number out of range") {
		t.Errorf("expected line-overflow warning, got: %q", logged)
	}
	if !strings.Contains(logged, "column number out of range") {
		t.Errorf("expected column-overflow warning, got: %q", logged)
	}
}

func TestParseGoVetOutput_noWarningOnCleanParse(t *testing.T) {
	// Inverse of the truncation test: a well-formed input must NOT emit a
	// warning. Guards against the easy regression where a future change
	// over-eagerly logs on every parse.
	logs := captureSlog(t, slog.LevelWarn)
	parseGoVetOutput("", "engine/agent/agent.go:10:5: issue\n")
	if logs.Len() != 0 {
		t.Errorf("clean parse must not emit warnings, got: %q", logs.String())
	}
}
