package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

type demarkusServerMatchesCase struct {
	name       string
	cmdline    string
	contentDir string // empty → use the default test dir
	want       bool
}

var demarkusServerMatchesDefaultDir = "/Users/fritz/latebit/NULLRUN/.project/memory"

var demarkusServerMatchesCases = []demarkusServerMatchesCase{
	{
		name:    "matches absolute path with exact -root",
		cmdline: "/Users/fritz/latebit/NULLRUN/.project/bin/demarkus-server -root " + demarkusServerMatchesDefaultDir + " -port 61254 -tokens /x",
		want:    true,
	},
	{
		name:    "matches relative executable",
		cmdline: "./demarkus-server -root " + demarkusServerMatchesDefaultDir + " -port 1",
		want:    true,
	},
	{
		name:    "matches plain binary name",
		cmdline: "demarkus-server -root " + demarkusServerMatchesDefaultDir,
		want:    true,
	},
	{
		name:    "rejects different executable",
		cmdline: "/usr/bin/other-binary -root " + demarkusServerMatchesDefaultDir,
		want:    false,
	},
	{
		name:    "rejects suffix-stripped executable",
		cmdline: "/bin/demarkus-server-v2 -root " + demarkusServerMatchesDefaultDir,
		want:    false,
	},
	{
		name:    "rejects different -root value",
		cmdline: "demarkus-server -root /tmp/other -port 1",
		want:    false,
	},
	{
		name:    "rejects path prefix overlap",
		cmdline: "demarkus-server -root " + demarkusServerMatchesDefaultDir + "-sibling -port 1",
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
	{
		// Paths with spaces are rare but legal. Argv boundaries are
		// lost in ps output, so demarkusServerMatches reconstructs
		// -root by joining tokens until the next flag.
		name:       "matches path containing spaces",
		cmdline:    "demarkus-server -root /tmp/foo bar/.project/memory -port 1 -tokens /x",
		contentDir: "/tmp/foo bar/.project/memory",
		want:       true,
	},
	{
		// Prefix overlap must still be rejected when paths contain
		// spaces — the sibling with a `-sibling` suffix reconstructs
		// to a different string than the target.
		name:       "rejects prefix overlap with spaces",
		cmdline:    "demarkus-server -root /tmp/foo bar-sibling/.project/memory -port 1",
		contentDir: "/tmp/foo bar/.project/memory",
		want:       false,
	},
	{
		// Path at end of argv (no trailing flags). The inner loop
		// must handle running off the end of tokens without panic.
		name:       "matches trailing spaced path at end of argv",
		cmdline:    "demarkus-server -root /tmp/foo bar/memory",
		contentDir: "/tmp/foo bar/memory",
		want:       true,
	},
}

func TestDemarkusServerMatches(t *testing.T) {
	for _, tc := range demarkusServerMatchesCases {
		t.Run(tc.name, func(t *testing.T) {
			cd := tc.contentDir
			if cd == "" {
				cd = demarkusServerMatchesDefaultDir
			}
			if got := demarkusServerMatches(tc.cmdline, cd); got != tc.want {
				t.Errorf("got %v, want %v for %q (contentDir=%q)", got, tc.want, tc.cmdline, cd)
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

func TestAcquireLock_BlocksSecondInstance(t *testing.T) {
	// Two Managers pointed at the same project directory must not be able
	// to both hold the lock. This is the single-instance guarantee that
	// prevents two junto processes from racing on — and clobbering — the
	// same memory server.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".project"), 0755); err != nil {
		t.Fatal(err)
	}

	a := New(root)
	b := New(root)

	if err := a.acquireLock(); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	t.Cleanup(a.releaseLock)

	err := b.acquireLock()
	if err == nil {
		b.releaseLock()
		t.Fatal("second acquire should have failed while first holds lock")
	}
	if !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Errorf("expected ErrInstanceAlreadyRunning, got: %v", err)
	}
}

func TestAcquireLock_SecondSucceedsAfterRelease(t *testing.T) {
	// After the first holder releases, the second must be able to
	// acquire. Confirms the lock doesn't leak past release.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".project"), 0755); err != nil {
		t.Fatal(err)
	}

	a := New(root)
	if err := a.acquireLock(); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	a.releaseLock()

	b := New(root)
	if err := b.acquireLock(); err != nil {
		t.Fatalf("second acquire after release should succeed: %v", err)
	}
	b.releaseLock()
}

func TestReleaseLock_Idempotent(t *testing.T) {
	// Stop() unconditionally calls releaseLock; tolerate repeat calls.
	m := New(t.TempDir())
	m.releaseLock() // no lock held — must be a no-op, not a panic
	m.releaseLock()
}

func TestReuseExisting_DoesNotKillRecycledUnrelatedPID(t *testing.T) {
	// reuseExisting must not terminate an adopted PID whose port probe
	// fails unless the PID is actually a demarkus-server for this
	// content dir. Otherwise a stale .memory-pid whose PID got recycled
	// by an unrelated process (editor, shell, etc.) would be SIGKILL'd
	// on the next junto launch — a real data-loss risk for the user.
	m := New(t.TempDir())

	// Set up state-files pointing at a live but unrelated PID.
	if err := os.MkdirAll(filepath.Join(m.projectRoot, ".project"), 0755); err != nil {
		t.Fatal(err)
	}
	// `sleep` stands in for "unrelated process we happen to share a PID with."
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn bystander: %v", err)
	}
	bystanderPID := cmd.Process.Pid
	bystanderDone := make(chan struct{})
	go func() {
		defer close(bystanderDone)
		// Wait error is discarded: we kill the bystander ourselves in
		// the test cleanup below, so the non-nil exit status is the
		// documented outcome, not a failure condition.
		_ = cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // best-effort — test is done with this process
		<-bystanderDone
	})

	if err := os.WriteFile(m.pidFile, []byte(strconv.Itoa(bystanderPID)), 0600); err != nil {
		t.Fatal(err)
	}
	// Port far above any real demarkus-server range to guarantee probe fail.
	if err := os.WriteFile(m.portFile, []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}

	// reuseExisting will: signal(0) bystanderPID → alive; probe port 1 → fail.
	// Old code (or unfixed code) would SIGKILL bystanderPID here. New code
	// must NOT, because bystanderPID is not a demarkus-server for contentDir.
	_, err := m.reuseExisting()
	if err == nil {
		t.Fatal("expected errNoExistingServer")
	}
	if !errors.Is(err, errNoExistingServer) {
		t.Errorf("expected errNoExistingServer, got: %v", err)
	}

	// The bystander must still be alive — proof we didn't kill it.
	proc, ferr := os.FindProcess(bystanderPID)
	if ferr != nil {
		t.Fatalf("FindProcess: %v", ferr)
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("bystander PID %d was killed by reuseExisting — this is the recycled-PID safety bug", bystanderPID)
	}
}

func TestStop_ReapsOwnedChildWithoutZombie(t *testing.T) {
	// Stop() on a Manager that owns a live child must fully reap the
	// child — not just signal it. Regression test for a prior bug where
	// owned-child bookkeeping (m.cmd/m.waitDone) was set only after
	// waitReady returned, so any early-startup failure took the
	// terminatePID path that signals but cannot reap, leaving a zombie
	// until junto exited.
	m := New(t.TempDir())

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	pid := cmd.Process.Pid

	// Mirror what startFresh() now does immediately after cmd.Start().
	m.cmd = cmd
	m.waitDone = make(chan struct{})
	go func() {
		defer close(m.waitDone)
		// Wait error is discarded: Stop() kills the child with
		// SIGTERM/SIGKILL, so a non-nil exit status is the documented
		// outcome. The goroutine's only job is to reap; the test
		// asserts the reap happened by checking for ESRCH below.
		_ = cmd.Wait()
	}()
	m.process = cmd.Process

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After reap, signal(0) to the (now-gone) PID must return ESRCH.
	// If Stop had fallen back to terminatePID without a Wait goroutine,
	// the child would linger as a zombie and signal(0) would succeed.
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Error("child still exists after Stop — zombie or unreaped")
	}
}

func TestTerminateOwnedChild_GracefulExit(t *testing.T) {
	// Spawn our own child and observe exit via the Wait-reap channel.
	// Unlike signal(0) polling, this path is immune to zombie state: the
	// Wait goroutine reaps the child as soon as it exits, so the done
	// channel fires before the graceful timeout even for children that
	// become momentarily defunct.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// cmd.Wait error is intentionally ignored: the process is killed
		// on purpose (SIGTERM/SIGKILL from terminateOwnedChild), so Wait
		// will return a non-nil "signal: terminated" / "signal: killed"
		// exit status. That is the expected outcome of the test, not a
		// failure, and the test's assertions are on terminateOwnedChild's
		// return and elapsed time — not on child exit code.
		_ = cmd.Wait()
	}()

	start := time.Now()
	if err := terminateOwnedChild(cmd.Process, done, 5*time.Second); err != nil {
		t.Fatalf("terminateOwnedChild: %v", err)
	}
	elapsed := time.Since(start)
	// SIGTERM on sleep exits within milliseconds; 1s ceiling catches
	// accidental escalation to SIGKILL or unnecessary waits.
	if elapsed > time.Second {
		t.Errorf("graceful termination too slow: %v", elapsed)
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
		// cmd.Wait error is intentionally ignored: terminatePID sends
		// SIGTERM/SIGKILL, so the non-nil "signal: terminated" exit
		// status is the expected outcome. The goroutine exists solely
		// to reap the child — without it, the zombie keeps signal(0)
		// returning nil and terminatePID's poll loop would wrongly
		// escalate to SIGKILL. Test assertions check elapsed time, not
		// exit status.
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
