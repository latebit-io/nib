package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	return New(root)
}

func TestEnsureBinariesShortCircuit(t *testing.T) {
	m := testManager(t)

	// Create the bin directory and all required binaries.
	if err := os.MkdirAll(m.binDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Split(requiredBins, ",") {
		path := filepath.Join(m.binDir, name)
		if err := os.WriteFile(path, []byte("fake"), 0755); err != nil {
			t.Fatalf("write fake binary %s: %v", name, err)
		}
	}

	// EnsureBinaries should return nil without attempting a download.
	if err := m.EnsureBinaries(); err != nil {
		t.Errorf("expected nil when all binaries present, got: %v", err)
	}
}

func TestCleanupFiles(t *testing.T) {
	m := testManager(t)

	dot := filepath.Join(m.projectRoot, ".project")
	if err := os.MkdirAll(dot, 0755); err != nil {
		t.Fatal(err)
	}

	// Create PID and port files.
	if err := os.WriteFile(m.pidFile, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.portFile, []byte("8080"), 0600); err != nil {
		t.Fatal(err)
	}

	m.cleanupFiles()

	if _, err := os.Stat(m.pidFile); !os.IsNotExist(err) {
		t.Errorf("PID file should be removed after cleanupFiles")
	}
	if _, err := os.Stat(m.portFile); !os.IsNotExist(err) {
		t.Errorf("port file should be removed after cleanupFiles")
	}
}

func TestProcessPID(t *testing.T) {
	t.Run("no PID file returns 0", func(t *testing.T) {
		m := testManager(t)
		if got := m.processPID(); got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})

	t.Run("reads PID from file", func(t *testing.T) {
		m := testManager(t)
		dot := filepath.Join(m.projectRoot, ".project")
		if err := os.MkdirAll(dot, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.pidFile, []byte("42\n"), 0600); err != nil {
			t.Fatal(err)
		}

		if got := m.processPID(); got != 42 {
			t.Errorf("expected 42, got %d", got)
		}
	})

	t.Run("in-memory process takes precedence", func(t *testing.T) {
		m := testManager(t)
		// Use our own process as a known-valid PID.
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		m.process = proc

		// Write a different PID to the file.
		dot := filepath.Join(m.projectRoot, ".project")
		if err := os.MkdirAll(dot, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.pidFile, []byte("99999"), 0600); err != nil {
			t.Fatal(err)
		}

		if got := m.processPID(); got != os.Getpid() {
			t.Errorf("expected %d (live process), got %d", os.Getpid(), got)
		}
	})
}

func TestReuseExisting(t *testing.T) {
	t.Run("no PID file returns errNoExistingServer", func(t *testing.T) {
		m := testManager(t)
		_, err := m.reuseExisting()
		if !errors.Is(err, errNoExistingServer) {
			t.Errorf("expected errNoExistingServer, got: %v", err)
		}
	})

	t.Run("stale PID returns errNoExistingServer and cleans up", func(t *testing.T) {
		m := testManager(t)
		dot := filepath.Join(m.projectRoot, ".project")
		if err := os.MkdirAll(dot, 0755); err != nil {
			t.Fatal(err)
		}

		// Use a PID that almost certainly doesn't exist.
		stalePID := 2147483647
		if err := os.WriteFile(m.pidFile, []byte(strconv.Itoa(stalePID)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.portFile, []byte("9999"), 0600); err != nil {
			t.Fatal(err)
		}

		_, err := m.reuseExisting()
		if !errors.Is(err, errNoExistingServer) {
			t.Errorf("expected errNoExistingServer, got: %v", err)
		}

		// Stale PID path should clean up files.
		if _, statErr := os.Stat(m.pidFile); !os.IsNotExist(statErr) {
			t.Error("PID file should be cleaned up after stale PID detection")
		}
		if _, statErr := os.Stat(m.portFile); !os.IsNotExist(statErr) {
			t.Error("port file should be cleaned up after stale PID detection")
		}
	})
}

func TestDemarkusServerMatches(t *testing.T) {
	contentDir := "/Users/fritz/latebit/NULLRUN/.project/memory"
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			name:    "matches absolute path with exact -root",
			cmdline: "/Users/fritz/latebit/NULLRUN/.project/bin/demarkus-server -root " + contentDir + " -port 61254 -tokens /x",
			want:    true,
		},
		{
			name:    "matches relative executable",
			cmdline: "./demarkus-server -root " + contentDir + " -port 1",
			want:    true,
		},
		{
			name:    "matches plain binary name",
			cmdline: "demarkus-server -root " + contentDir,
			want:    true,
		},
		{
			name:    "rejects different executable",
			cmdline: "/usr/bin/other-binary -root " + contentDir,
			want:    false,
		},
		{
			name:    "rejects suffix-stripped executable",
			cmdline: "/bin/demarkus-server-v2 -root " + contentDir,
			want:    false,
		},
		{
			name:    "rejects different -root value",
			cmdline: "demarkus-server -root /tmp/other -port 1",
			want:    false,
		},
		{
			name:    "rejects path prefix overlap",
			cmdline: "demarkus-server -root " + contentDir + "-sibling -port 1",
			want:    false,
		},
		{
			name:    "rejects missing -root flag",
			cmdline: "demarkus-server -port 1",
			want:    false,
		},
		{
			name:    "rejects empty cmdline",
			cmdline: "",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := demarkusServerMatches(tc.cmdline, contentDir); got != tc.want {
				t.Errorf("got %v, want %v for %q", got, tc.want, tc.cmdline)
			}
		})
	}
}

func TestFindDemarkusServerPIDs_NoMatches(t *testing.T) {
	// A content dir that cannot possibly match anything in the live ps
	// listing — uses a tmp-scoped path. Confirms the happy case when no
	// orphans exist.
	tmp := t.TempDir()
	pids, err := findDemarkusServerPIDs(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pids) != 0 {
		t.Errorf("expected no PIDs for unique tmp dir, got: %v", pids)
	}
}

func TestTerminatePID_AlreadyGone(t *testing.T) {
	// A PID unlikely to exist. terminatePID should return an error from
	// the initial SIGTERM (ESRCH) — surfacing is the documented contract.
	err := terminatePID(2147483647, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for nonexistent PID")
	}
}

func TestTerminatePID_GracefulExit(t *testing.T) {
	// `sleep` exits promptly on SIGTERM. Release() + a goroutine that
	// reaps on exit mimics production: demarkus-server is Release()'d at
	// Start, so when it exits it is reaped by init and signal(0) returns
	// ESRCH promptly. Without either, the test-spawned child lingers as
	// a zombie, signal(0) keeps succeeding, and the poll loop falsely
	// escalates to SIGKILL.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() { <-done })

	start := time.Now()
	if err := terminatePID(pid, 5*time.Second); err != nil {
		t.Fatalf("terminatePID: %v", err)
	}
	elapsed := time.Since(start)
	// Graceful path should complete well under the 5s cap. 1s ceiling
	// catches any regression that forces escalation to SIGKILL.
	if elapsed > time.Second {
		t.Errorf("graceful termination took too long: %v", elapsed)
	}
}
