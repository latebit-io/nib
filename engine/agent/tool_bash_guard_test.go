package agent

import (
	"strings"
	"testing"
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
		{name: "grep pattern", command: "grep -r TODO .", blocked: false},
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
		{name: "perl in-place", command: "perl -i -pe 's/old/new/' file.go", blocked: true},
		{name: "perl combined -pi", command: "perl -pi -e 's/old/new/' file.go", blocked: true},
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
