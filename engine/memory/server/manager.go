// Package server manages the demarkus-server child process lifecycle.
// It handles startup, shutdown, port selection, PID tracking, and token bootstrap.
package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Manager manages the demarkus-server child process.
type Manager struct {
	projectRoot string
	binDir      string // .project/bin/
	contentDir  string // .project/memory/
	tokensFile  string // .project/.memory-tokens.toml
	pidFile     string // .project/.memory-pid
	portFile    string // .project/.memory-port
	tokenFile   string // .project/.memory-token

	port    int
	process *os.Process
}

// New creates a Manager for the given project root.
func New(projectRoot string) *Manager {
	dot := filepath.Join(projectRoot, ".project")
	return &Manager{
		projectRoot: projectRoot,
		binDir:      filepath.Join(dot, "bin"),
		contentDir:  filepath.Join(dot, "memory"),
		tokensFile:  filepath.Join(dot, ".memory-tokens.toml"),
		pidFile:     filepath.Join(dot, ".memory-pid"),
		portFile:    filepath.Join(dot, ".memory-port"),
		tokenFile:   filepath.Join(dot, ".memory-token"),
	}
}

// Start launches the demarkus-server. Returns the port it's listening on.
// If a server is already running (detected via PID file), reuses it.
func (m *Manager) Start() (int, error) {
	// Ensure content directory exists.
	if err := os.MkdirAll(m.contentDir, 0755); err != nil {
		return 0, fmt.Errorf("memory server: create content dir: %w", err)
	}

	// Check for existing server via PID file.
	if port, err := m.reuseExisting(); err == nil {
		m.port = port
		slog.Info("memory server: reusing existing", "port", port)
		return port, nil
	}

	// Find a free port.
	port, err := freePort()
	if err != nil {
		return 0, fmt.Errorf("memory server: find free port: %w", err)
	}

	serverBin := filepath.Join(m.binDir, "demarkus-server")

	args := []string{
		"-root", m.contentDir,
		"-port", strconv.Itoa(port),
	}
	if _, err := os.Stat(m.tokensFile); err == nil {
		args = append(args, "-tokens", m.tokensFile)
	}

	cmd := exec.Command(serverBin, args...)
	// Detach from parent process group so the server survives if Junto crashes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("memory server: start: %w", err)
	}

	m.process = cmd.Process
	m.port = port

	// Write PID and port files.
	if err := os.WriteFile(m.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		slog.Warn("memory server: write PID file", "err", err)
	}
	if err := os.WriteFile(m.portFile, []byte(strconv.Itoa(port)), 0600); err != nil {
		slog.Warn("memory server: write port file", "err", err)
	}

	// Wait for server to become ready.
	if err := m.waitReady(port); err != nil {
		// Server failed to start — clean up.
		_ = m.Stop()
		return 0, fmt.Errorf("memory server: not ready: %w", err)
	}

	// Release the process so we don't leak a zombie if Junto exits without Stop().
	// The PID file lets us find it again.
	_ = cmd.Process.Release()

	slog.Info("memory server: started", "port", port, "pid", cmd.Process.Pid)
	return port, nil
}

// Stop kills the server process and cleans up PID and port files.
func (m *Manager) Stop() error {
	pid := m.processPID()
	if pid == 0 {
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		m.cleanupFiles()
		return nil
	}

	// SIGTERM for graceful shutdown.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process already dead.
		m.cleanupFiles()
		return nil
	}

	// Wait up to 5s for graceful shutdown.
	done := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Graceful shutdown succeeded.
	case <-time.After(5 * time.Second):
		// Force kill.
		_ = proc.Signal(syscall.SIGKILL)
		<-done
	}

	m.cleanupFiles()
	m.process = nil
	m.port = 0
	slog.Info("memory server: stopped", "pid", pid)
	return nil
}

// Address returns the mark:// URL for the running server.
func (m *Manager) Address() string {
	return fmt.Sprintf("mark://localhost:%d", m.port)
}

// Port returns the port the server is listening on.
func (m *Manager) Port() int {
	return m.port
}

// EnsureBinaries checks .project/bin/ for demarkus binaries.
// If any are missing, downloads and installs them from GitHub releases.
func (m *Manager) EnsureBinaries() error {
	for _, name := range strings.Split(requiredBins, ",") {
		path := filepath.Join(m.binDir, name)
		if _, err := os.Stat(path); err != nil {
			versionFile := filepath.Join(m.projectRoot, ".project", ".memory-version")
			return install(m.binDir, versionFile)
		}
	}
	return nil
}

// EnsureToken generates an auth token if one doesn't exist.
// Returns the raw token string.
func (m *Manager) EnsureToken() (string, error) {
	// Check for existing token.
	if data, err := os.ReadFile(m.tokenFile); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	}

	slog.Info("memory server: generating auth token")

	tokenBin := filepath.Join(m.binDir, "demarkus-token")
	cmd := exec.Command(tokenBin, "generate",
		"-label", "junto",
		"-paths", "/**",
		"-ops", "publish,append",
		"-tokens", m.tokensFile,
	)

	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("memory server: generate token: %w", err)
	}

	// Raw token is printed to stdout.
	rawToken := strings.TrimSpace(outBuf.String())
	if rawToken == "" {
		return "", errors.New("memory server: token generation produced empty output")
	}

	// Persist raw token (gitignored).
	if err := os.WriteFile(m.tokenFile, []byte(rawToken), 0600); err != nil {
		return "", fmt.Errorf("memory server: write token file: %w", err)
	}

	return rawToken, nil
}

// CheckUpgrade queries GitHub for the latest release and compares to .memory-version.
// Returns (newVersion, needsUpgrade, error).
func (m *Manager) CheckUpgrade() (string, bool, error) {
	versionFile := filepath.Join(m.projectRoot, ".project", ".memory-version")
	current, err := os.ReadFile(versionFile)
	if err != nil {
		// No version file — can't check.
		return "", false, nil
	}
	currentVersion := strings.TrimSpace(string(current))
	if currentVersion == "" {
		return "", false, nil
	}

	// Shell out to the install script with a dry-run style check.
	// For now, we skip the upgrade check to keep things simple —
	// the install script handles version pinning. The upgrade prompt
	// can be implemented in a future iteration.
	return "", false, nil
}

// reuseExisting checks for a running server via the PID file.
// Returns the port if the server is alive, or an error otherwise.
func (m *Manager) reuseExisting() (int, error) {
	pidData, err := os.ReadFile(m.pidFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return 0, err
	}

	// Check if process is alive (signal 0 doesn't kill, just checks).
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, err
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is dead — clean up stale files.
		m.cleanupFiles()
		return 0, fmt.Errorf("stale PID %d", pid)
	}

	// Process is alive — read port.
	portData, err := os.ReadFile(m.portFile)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(portData)))
	if err != nil {
		return 0, err
	}

	m.process = proc
	return port, nil
}

// waitReady polls the server using the demarkus CLI until it responds or the timeout expires.
// Demarkus uses QUIC (UDP + TLS), so a simple TCP/UDP dial isn't sufficient.
func (m *Manager) waitReady(port int) error {
	clientBin := filepath.Join(m.binDir, "demarkus")
	url := fmt.Sprintf("mark://localhost:%d/", port)
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		cmd := exec.Command(clientBin, "-insecure", "-no-cache", url)
		if err := cmd.Run(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server not ready on port %d after 5s", port)
}

// processPID returns the PID of the managed server, or 0 if unknown.
func (m *Manager) processPID() int {
	if m.process != nil {
		return m.process.Pid
	}
	data, err := os.ReadFile(m.pidFile)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// cleanupFiles removes PID and port files.
func (m *Manager) cleanupFiles() {
	_ = os.Remove(m.pidFile)
	_ = os.Remove(m.portFile)
}

// freePort finds an available port by binding to :0 and reading the assigned port.
func freePort() (int, error) {
	l, err := net.ListenPacket("udp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }() // best-effort — port already captured
	return l.LocalAddr().(*net.UDPAddr).Port, nil
}
