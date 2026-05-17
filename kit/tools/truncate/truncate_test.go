package truncate

import (
	"errors"
	"strings"
	"testing"
)

type recordingSink struct {
	calls    int
	lastBody string
	path     string
	err      error
}

func (r *recordingSink) Stash(label, content string) (string, error) {
	r.calls++
	r.lastBody = content
	return r.path, r.err
}

func TestBytes_BelowCapPassthrough(t *testing.T) {
	t.Parallel()
	in := "hello world"
	sink := &recordingSink{path: "stash/x.txt"}

	out, truncated := Bytes("test", in, 100, sink)

	if truncated {
		t.Fatalf("expected truncated=false for input below cap")
	}
	if out != in {
		t.Fatalf("expected passthrough, got %q", out)
	}
	if sink.calls != 0 {
		t.Fatalf("expected sink not invoked when below cap, got %d calls", sink.calls)
	}
}

func TestBytes_AtCapPassthrough(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("a", 100)
	out, truncated := Bytes("test", in, 100, nil)
	if truncated {
		t.Fatalf("expected truncated=false when len == cap")
	}
	if out != in {
		t.Fatalf("expected passthrough")
	}
}

func TestBytes_AboveCapWithSink(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("a", 5000)
	sink := &recordingSink{path: ".project/tooltmp/abcd/0001-bash.txt"}

	out, truncated := Bytes("bash", in, 1000, sink)

	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if sink.calls != 1 {
		t.Fatalf("expected sink invoked exactly once, got %d", sink.calls)
	}
	if sink.lastBody != in {
		t.Fatalf("expected sink to receive full pre-truncation content")
	}
	if !strings.Contains(out, "Full output: .project/tooltmp/abcd/0001-bash.txt") {
		t.Fatalf("marker missing stash path: %q", out)
	}
	if !strings.HasSuffix(out, "]") {
		t.Fatalf("marker missing closing bracket")
	}
	if !strings.Contains(out, "[Truncated: showing 1000 of 5000 bytes") {
		t.Fatalf("marker missing showing/total: %q", out)
	}
}

func TestBytes_AboveCapNilSink(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("b", 200)
	out, truncated := Bytes("test", in, 50, nil)
	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if strings.Contains(out, "Full output:") {
		t.Fatalf("marker should omit Full-output when sink is nil: %q", out)
	}
}

func TestBytes_StashErrorIsSwallowed(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("c", 200)
	sink := &recordingSink{err: errors.New("disk full")}

	out, truncated := Bytes("test", in, 50, sink)

	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if strings.Contains(out, "Full output:") {
		t.Fatalf("marker should omit Full-output on sink error: %q", out)
	}
	if sink.calls != 1 {
		t.Fatalf("sink should have been invoked once; got %d", sink.calls)
	}
}

func TestBytes_UTF8BoundaryPreserved(t *testing.T) {
	t.Parallel()
	// Each `é` is 2 UTF-8 bytes. Place a multibyte rune straddling
	// the cap so a naive slice would corrupt it.
	in := strings.Repeat("a", 49) + "é" + strings.Repeat("b", 100)
	cap := 50 // would land mid-rune

	out, truncated := Bytes("test", in, cap, nil)

	if !truncated {
		t.Fatal("expected truncated=true")
	}
	body := strings.TrimSuffix(out, marker(out))
	if !isValidUTF8(body) {
		t.Fatalf("truncated body is not valid UTF-8: %q", body)
	}
	if len(body) > cap {
		t.Fatalf("body length %d exceeds cap %d", len(body), cap)
	}
}

func TestLines_BelowCapPassthrough(t *testing.T) {
	t.Parallel()
	in := "a\nb\nc\n"
	out, truncated := Lines("test", in, 5, nil)
	if truncated {
		t.Fatalf("expected truncated=false")
	}
	if out != in {
		t.Fatalf("expected passthrough, got %q", out)
	}
}

func TestLines_TrailingNewlineNotCounted(t *testing.T) {
	t.Parallel()
	// 3 content lines + trailing newline. With cap=3 this must NOT
	// trigger truncation: the empty trailing entry is a final newline,
	// not a fourth line.
	in := "x\ny\nz\n"
	out, truncated := Lines("test", in, 3, nil)
	if truncated {
		t.Fatalf("trailing newline should not push count past cap")
	}
	if out != in {
		t.Fatalf("expected passthrough")
	}
}

func TestLines_AboveCap(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("line\n", 100)
	sink := &recordingSink{path: ".project/tooltmp/run/0001-read.txt"}

	out, truncated := Lines("read", in, 10, sink)

	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if sink.calls != 1 {
		t.Fatalf("expected sink invoked once, got %d", sink.calls)
	}
	if !strings.Contains(out, "[Truncated: showing 10 of 100 lines") {
		t.Fatalf("marker missing line counts: %q", out)
	}
	if !strings.Contains(out, "Full output: .project/tooltmp/run/0001-read.txt") {
		t.Fatalf("marker missing stash path: %q", out)
	}
	// Head should contain exactly 10 lines, each "line\n".
	head := strings.TrimSuffix(out, markerOfLines(10, 100, ".project/tooltmp/run/0001-read.txt"))
	got := strings.Count(head, "\n")
	if got != 10 {
		t.Fatalf("expected 10 newlines in head, got %d", got)
	}
}

func TestMarker_FormatsBothShapes(t *testing.T) {
	t.Parallel()
	with := Marker(1000, 5000, "bytes", "p")
	if !strings.Contains(with, "Full output: p") {
		t.Fatalf("marker w/ path missing Full output: %q", with)
	}
	without := Marker(1000, 5000, "bytes", "")
	if strings.Contains(without, "Full output:") {
		t.Fatalf("marker w/o path should omit Full output: %q", without)
	}
}

func TestBytes_DeterministicWithoutSink(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("z", 5000)
	a, ta := Bytes("test", in, 1000, nil)
	b, tb := Bytes("test", in, 1000, nil)
	if a != b || ta != tb {
		t.Fatal("Bytes must be deterministic for identical inputs without a sink")
	}
}

// marker isolates the trailing "\n\n[Truncated...]" suffix so tests
// can split body from marker without re-implementing format strings.
func marker(s string) string {
	i := strings.LastIndex(s, "\n\n[Truncated:")
	if i < 0 {
		return ""
	}
	return s[i:]
}

func markerOfLines(showing, total int, path string) string {
	return Marker(showing, total, "lines", path)
}

// isValidUTF8 is the test-local UTF-8 validator (avoids importing
// unicode/utf8 in the package proper if we ever choose to).
func isValidUTF8(s string) bool {
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b < 0x80:
			i++
		case b < 0xC0:
			return false
		case b < 0xE0:
			if i+1 >= len(s) || s[i+1]&0xC0 != 0x80 {
				return false
			}
			i += 2
		case b < 0xF0:
			if i+2 >= len(s) || s[i+1]&0xC0 != 0x80 || s[i+2]&0xC0 != 0x80 {
				return false
			}
			i += 3
		case b < 0xF8:
			if i+3 >= len(s) || s[i+1]&0xC0 != 0x80 || s[i+2]&0xC0 != 0x80 || s[i+3]&0xC0 != 0x80 {
				return false
			}
			i += 4
		default:
			return false
		}
	}
	return true
}
