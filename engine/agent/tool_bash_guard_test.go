package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFileWriteGuard(t *testing.T) {
	tests := []struct {
		name    string
		command string
		blocked bool // true if guard should return non-empty error
	}{
		// --- Allowed commands ---
		{name: "simple build", command: "go build ./...", blocked: false},
		{name: "run tests", command: "go test ./...", blocked: false},
		{name: "gofmt check", command: "gofmt -l .", blocked: false},
		{name: "pipe", command: "go test ./... | head -20", blocked: false},
		{name: "echo no redirect", command: "echo hello", blocked: false},
		{name: "stderr to stdout", command: "go build ./... 2>&1", blocked: false},
		{name: "stdout to devnull", command: "go build ./... > /dev/null", blocked: false},
		{name: "stderr to devnull", command: "go test ./... 2>/dev/null", blocked: false},
		{name: "both to devnull", command: "cmd > /dev/null 2>&1", blocked: false},
		{name: "redirect to dev stdout", command: "echo test > /dev/stdout", blocked: false},
		{name: "redirect to dev stderr", command: "echo test > /dev/stderr", blocked: false},
		{name: "redirect to tmp", command: "go test -v ./... > /tmp/test-output.log", blocked: false},
		{name: "redirect to quoted tmp", command: `go test -v ./... > "/tmp/test-output.log"`, blocked: false},
		{name: "redirect to TMPDIR", command: "go test -v > $TMPDIR/out.log", blocked: true},
		{name: "tee to devnull", command: "go test ./... | tee /dev/null", blocked: false},
		{name: "tee to tmp", command: "go test ./... | tee /tmp/out.log", blocked: false},
		{name: "process substitution", command: "diff <(cmd1) <(cmd2)", blocked: false},
		{name: "output process substitution", command: "cmd > >(tee /dev/null)", blocked: false},
		{name: "git diff", command: "git diff HEAD~1", blocked: false},
		{name: "make target", command: "make build", blocked: false},
		{name: "fd redirect devfd", command: "cmd > /dev/fd/1", blocked: false},

		// --- Blocked commands ---
		{name: "redirect to file", command: "echo hello > file.txt", blocked: true},
		{name: "append to file", command: "echo hello >> file.txt", blocked: true},
		{name: "head to file", command: "head -n -1 tui_view.go > /tmp_view.go", blocked: true},
		{name: "cat heredoc to file", command: "cat << EOF > main.go", blocked: true},
		{name: "redirect to go file", command: "gofmt -w . > output.go", blocked: true},
		{name: "sed in-place", command: "sed -i 's/old/new/' file.go", blocked: true},
		{name: "sed in-place backup", command: "sed -i.bak 's/old/new/' file.go", blocked: true},
		{name: "sed in-place backup no dot", command: "sed -ibak 's/old/new/' file.go", blocked: true},
		{name: "perl in-place", command: "perl -i -pe 's/old/new/' file.go", blocked: true},
		{name: "perl combined -pi", command: "perl -pi -e 's/old/new/' file.go", blocked: true},
		{name: "perl combined -ip", command: "perl -ip -e 's/old/new/' file.go", blocked: true},
		{name: "perl -0777pi", command: "perl -0777pi -e 's/old/new/' file.go", blocked: true},
		{name: "sed with flags then -i", command: "sed -E -i 's/old/new/' file.go", blocked: true},
		{name: "sed combined -ni", command: "sed -ni 's/old/new/p' file.go", blocked: true},
		{name: "sed --in-place", command: "sed --in-place 's/old/new/' file.go", blocked: true},
		{name: "tee to project file", command: "echo test | tee output.log", blocked: true},
		{name: "tee append to file", command: "echo test | tee -a build.log", blocked: true},
		{name: "redirect to relative path", command: "ls > listing.txt", blocked: true},
		{name: "redirect to subdir", command: "echo test > src/file.go", blocked: true},
		{name: "clobber redirect", command: "echo test >| file.txt", blocked: true},
		{name: "clobber no space", command: "echo test >|file.txt", blocked: true},

		// --- Heuristic catches these despite indirect intent ---
		{name: "sed with command sub containing -i", command: "sed $(echo -i) 's/x/y/' file.go", blocked: true},
		{name: "redirect to variable", command: "f=file.go; echo x > $f", blocked: true},

		// --- Known bypass vectors (accepted risk, documented) ---
		// Only genuine bypasses that require a full shell parser to detect.
		{name: "bypass: tee safe then unsafe", command: "cmd | tee /dev/null file2.go", blocked: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fileWriteGuard(tt.command)
			if tt.blocked && result == "" {
				t.Errorf("expected command to be blocked: %s", tt.command)
			}
			if !tt.blocked && result != "" {
				t.Errorf("expected command to be allowed, got: %s\ncommand: %s", result, tt.command)
			}
		})
	}
}

func TestFileWriteGuardErrorMessage(t *testing.T) {
	// Verify the error message includes the target for debuggability.
	result := fileWriteGuard("echo hello > output.txt")
	if result == "" {
		t.Fatal("expected non-empty error")
	}
	// Should mention the file and the alternative tools.
	for _, want := range []string{"output.txt", "edit_file", "write_file"} {
		if !strings.Contains(result, want) {
			t.Errorf("error message missing %q: %s", want, result)
		}
	}
}

// TestSearchCommandGuard locks in the 2026-04-26 rule that bash must not be
// the route for code search. The dedicated tools (search_project, glob)
// return structured results and respect gitignore; bash grep/rg/find dump
// raw text, drag the conversation through walls of output, and miss
// gitignored files. Each blocked-row checks both that the command is
// rejected and that the error names the tool we want the LLM to use
// instead.
func TestSearchCommandGuard(t *testing.T) {
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Allowed — non-search bash commands stay allowed.
		{"build", "go build ./...", false},
		{"echo no search tool", "echo grep is a word", false},
		{"go test", "go test ./agent/...", false},
		{"make build", "make build", false},

		// Blocked — direct invocations.
		{"grep", "grep -r TODO .", true},
		{"rg", "rg pattern", true},
		{"ripgrep", "ripgrep pattern", true},
		{"ag", "ag pattern .", true},
		{"ack", "ack TODO", true},
		{"find", "find . -name '*.go'", true},

		// Blocked — chained after separators (catches the model trying to
		// sneak grep through a `cd && grep` pipeline).
		{"chained with &&", "cd src && grep TODO .", true},
		{"chained with ;", "cd src; grep TODO .", true},
		{"piped from another command", "cat *.go | grep TODO", true},

		// Blocked — newline-separated multi-line commands (e.g.
		// pasted into bash -c with a heredoc). Without (?m) and \n
		// in the boundary class these slipped through.
		{"newline-separated", "cd src\ngrep TODO .", true},
		{"newline at start of second tool", "echo hi\nfind . -name '*.go'", true},
		{"newline + spaces before tool", "set -e\n  rg pattern", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := searchCommandGuard(tt.command)
			if tt.blocked && result == "" {
				t.Errorf("expected blocked: %s", tt.command)
			}
			if !tt.blocked && result != "" {
				t.Errorf("expected allowed but got: %s\ncommand: %s", result, tt.command)
			}
			if tt.blocked && !strings.Contains(result, "search_project") {
				t.Errorf("blocked-search error should name search_project: %s", result)
			}
		})
	}
}

// TestDestructiveCommandGuard locks in the policy that an autonomous agent
// does not run history-loss or workspace-loss operations without explicit
// developer ask. The blocked commands have all caused real lost work in
// other agent harnesses; refusing at the tool layer is cheaper than
// auditing them after the fact. The allowed rows protect against
// over-blocking — `rm path` is fine for single files, `git status` and
// other read-only git commands are not destructive.
func TestDestructiveCommandGuard(t *testing.T) {
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Allowed.
		{"rm single file", "rm /tmp/scratch.log", false},
		{"rm with -f only", "rm -f /tmp/scratch.log", false},
		{"rm with --recursive alone (interactive)", "rm --recursive build/", false},
		{"git status", "git status", false},
		{"git diff", "git diff HEAD~1", false},
		{"git log", "git log --oneline -5", false},
		{"git fetch", "git fetch", false},

		// Blocked — recursive force delete in any flag order.
		{"rm -rf", "rm -rf build/", true},
		{"rm -fr", "rm -fr build/", true},
		{"rm with -rf among other flags", "rm -vrf build/", true},
		{"rm long flags", "rm --recursive --force build/", true},
		{"rm long flags reversed", "rm --force --recursive build/", true},

		// Blocked — recursive + force split across separate short-flag tokens.
		// Common idiom; prior regex only matched flags joined into one
		// token, leaving these two orderings as a known bypass.
		{"rm split -r -f", "rm -r -f build/", true},
		{"rm split -f -r", "rm -f -r build/", true},
		{"rm split with extra flag between", "rm -r -v -f build/", true},
		{"rm split with prepended -v", "rm -v -r -f build/", true},
		// But a separator between r and f flags must NOT cross-fire:
		// `rm -r foo; cmd -f bar` is rm of one item then a separate cmd.
		{"rm -r then separator then -f not cross-fired", "rm -r foo; cmd -f bar", false},

		// Blocked — git destructive ops.
		{"git push", "git push origin main", true},
		{"git push force", "git push --force origin main", true},
		{"git checkout file", "git checkout -- file.go", true},
		{"git checkout branch", "git checkout main", true},
		{"git switch", "git switch main", true},
		{"git reset hard", "git reset --hard HEAD~1", true},
		{"git clean -fd", "git clean -fd", true},
		{"git clean --force", "git clean --force", true},
		{"git clean --force -d", "git clean --force -d", true},
		{"git clean -f -d", "git clean -f -d", true},

		// Newline-separated destructive variants. Bash treats newlines as
		// statement separators by default, but a model that emits a
		// multi-line tool argument may produce a destructive sequence
		// that is one logical command in some invocation contexts (bash
		// -c with embedded newlines, heredocs, line-continuation). The
		// guard's job is to be conservative — block the recognisable
		// shape rather than reasoning about how bash will tokenise.
		{"rm long flags newline separated", "rm --recursive\n--force build/", true},
		{"rm split flags newline separated", "rm -r\n-f build/", true},
		{"git reset hard newline", "git reset\n--hard HEAD", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := destructiveCommandGuard(tt.command)
			if tt.blocked && result == "" {
				t.Errorf("expected blocked: %s", tt.command)
			}
			if !tt.blocked && result != "" {
				t.Errorf("expected allowed but got: %s\ncommand: %s", result, tt.command)
			}
		})
	}
}

// TestRedactCommandPreview locks in the rune-aware truncation contract:
// the byte cap must NOT split a multibyte UTF-8 sequence. Without this,
// a path containing a CJK glyph or em-dash whose rune-start happens to
// land just before the cap would yield invalid UTF-8 in slog output —
// some log backends drop or re-encode such strings, hiding the very
// signal we wanted preserved. Mirrors the pattern at
// engine/agent/prompt.go:51.
func TestRedactCommandPreview(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          string
		wantSuffix  string // expected suffix of the result; empty means no truncation
		wantPrefix  string // result must begin with this
		wantValid   bool   // result must be valid UTF-8
		shouldEqual string // when set, result must equal this exactly
	}{
		{
			name:        "short ASCII passes through trimmed",
			in:          "  go build ./...  ",
			shouldEqual: "go build ./...",
			wantValid:   true,
		},
		{
			name:       "long ASCII truncates with ellipsis",
			in:         strings.Repeat("a", 100),
			wantPrefix: strings.Repeat("a", 64),
			wantSuffix: "…",
			wantValid:  true,
		},
		{
			name: "multibyte glyph at cap boundary backs up to rune start",
			// Build an input where byte index redactPreviewBytes (=64) lands
			// inside a 3-byte CJK rune. 63 ASCII bytes + a 3-byte glyph (世
			// is 3 bytes in UTF-8) + filler. The cut at 64 would be in the
			// middle of 世 (bytes 64,65 of the rune); the function must
			// back up to position 63 so the returned string ends just
			// before 世 and remains valid UTF-8.
			in:         strings.Repeat("a", 63) + "世" + strings.Repeat("b", 30),
			wantPrefix: strings.Repeat("a", 63),
			wantSuffix: "…",
			wantValid:  true,
		},
		{
			name: "all multibyte content truncates cleanly",
			// 30 copies of 世 = 90 bytes. Cut at 64 lands inside the
			// 22nd rune. Function backs up to a rune boundary and
			// appends the ellipsis.
			in:         strings.Repeat("世", 30),
			wantPrefix: strings.Repeat("世", 21), // 21 * 3 = 63 bytes <= 64
			wantSuffix: "…",
			wantValid:  true,
		},

		// --- Env-var masking (added 2026-04-27) ---
		// Inline KEY=VALUE assignments are the most common shape under
		// which secrets leak into shell-blocked logs. Replace VALUE
		// with <redacted>; leave the rest of the command intact so the
		// recognisable shape survives.
		{
			name:        "leading env assignment masked",
			in:          "OPENAI_API_KEY=sk-abc123 ./script.sh",
			shouldEqual: "OPENAI_API_KEY=<redacted> ./script.sh",
			wantValid:   true,
		},
		{
			name:        "multiple assignments all masked",
			in:          "FOO=bar BAZ=qux ./run",
			shouldEqual: "FOO=<redacted> BAZ=<redacted> ./run",
			wantValid:   true,
		},
		{
			name:        "non-secret env still over-redacted (acceptable)",
			in:          "PATH=/usr/bin echo hi",
			shouldEqual: "PATH=<redacted> echo hi",
			wantValid:   true,
		},
		{
			name:        "no equals sign — no masking",
			in:          "echo hello",
			shouldEqual: "echo hello",
			wantValid:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := redactCommandPreview(tt.in)
			if tt.shouldEqual != "" && got != tt.shouldEqual {
				t.Errorf("got %q, want %q", got, tt.shouldEqual)
			}
			if tt.wantPrefix != "" && !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("got %q, want prefix %q", got, tt.wantPrefix)
			}
			if tt.wantSuffix != "" && !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("got %q, want suffix %q", got, tt.wantSuffix)
			}
			if tt.wantValid && !utf8.ValidString(got) {
				t.Errorf("got %q is not valid UTF-8", got)
			}
		})
	}
}

// TestGuardCommand verifies the unified entry point composes the three
// individual guards and returns the first error encountered. Tests one
// row from each guard family so a regression in [guardCommand]'s
// dispatching is caught even if individual guards still pass.
func TestGuardCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"allowed build", "go build ./...", false},
		{"file write blocked", "echo x > out.txt", true},
		{"search blocked", "grep TODO .", true},
		{"destructive blocked", "rm -rf build/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := guardCommand(tt.command)
			if tt.blocked && result == "" {
				t.Errorf("expected blocked: %s", tt.command)
			}
			if !tt.blocked && result != "" {
				t.Errorf("expected allowed but got: %s\ncommand: %s", result, tt.command)
			}
		})
	}
}
