package server

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
