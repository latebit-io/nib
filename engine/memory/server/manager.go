// Package server manages the demarkus-server child process lifecycle.
// It handles startup, shutdown, port selection, PID tracking, and token bootstrap.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/junto/engine/mcp"
	"github.com/latebit-io/junto/engine/memory"
	"github.com/latebit-io/junto/engine/memory/mcpadapter"
)

// Manager manages the demarkus-server child process and the demarkus-mcp
// client subprocess that fronts it.
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

	// mcpClient is the demarkus-mcp subprocess created by NewStore. Closed by
	// Stop so the subprocess doesn't outlive the server it talks to.
	mcpClient *mcp.Client
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
// Any orphaned demarkus-server processes bound to this project's content
// directory are reaped before returning — unresponsive servers from
// earlier sessions (e.g. junto crashed before Stop() ran, or the PID file
// was lost) would otherwise accumulate alongside the live one.
func (m *Manager) Start() (int, error) {
	// Ensure content directory exists.
	if err := os.MkdirAll(m.contentDir, 0755); err != nil {
		return 0, fmt.Errorf("memory server: create content dir: %w", err)
	}

	// Check for existing server via PID file.
	port, reuseErr := m.reuseExisting()
	if reuseErr == nil {
		m.port = port
		// Reuse path: another demarkus-server for this content dir is an
		// orphan. Spare only the PID we just adopted.
		m.reapOrphans(m.processPID())
		slog.Info("memory server: reusing existing", "port", port)
		return port, nil
	}
	if !errors.Is(reuseErr, errNoExistingServer) {
		// A live process was found but something else failed (port file, probe).
		// Do not launch a second server against the same content root.
		return 0, fmt.Errorf("memory server: existing server detected but unusable: %w", reuseErr)
	}

	// No existing server — reap any untracked orphans before launching
	// fresh. Without this, a prior session that lost its PID file would
	// leave a zombie that accumulates indefinitely.
	m.reapOrphans(0)

	// Find a free port.
	port, err := freePort()
	if err != nil {
		return 0, fmt.Errorf("memory server: find free port: %w", err)
	}

	serverBin := filepath.Join(m.binDir, "demarkus-server")

	// Tokens file must exist — EnsureToken must be called before Start.
	if _, err := os.Stat(m.tokensFile); err != nil {
		return 0, fmt.Errorf("memory server: tokens file missing (call EnsureToken first): %w", err)
	}

	args := []string{
		"-root", m.contentDir,
		"-port", strconv.Itoa(port),
		"-tokens", m.tokensFile,
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

	// Write PID and port files — these are required for reuse and stop.
	// If either fails, kill the server to avoid orphaning it.
	if err := os.WriteFile(m.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			slog.Warn("memory server: kill after PID write failure", "killErr", killErr)
		}
		return 0, fmt.Errorf("memory server: write PID file: %w", err)
	}
	if err := os.WriteFile(m.portFile, []byte(strconv.Itoa(port)), 0600); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			slog.Warn("memory server: kill after port write failure", "killErr", killErr)
		}
		m.cleanupFiles()
		return 0, fmt.Errorf("memory server: write port file: %w", err)
	}

	// Wait for server to become ready.
	if err := m.waitReady(port); err != nil {
		if stopErr := m.Stop(); stopErr != nil {
			slog.Warn("memory server: stop after readiness failure", "stopErr", stopErr)
		}
		return 0, fmt.Errorf("memory server: not ready: %w", err)
	}

	// Release the process so we don't leak a zombie if Junto exits without Stop().
	// The PID file lets us find it again. Release cannot meaningfully fail here —
	// the process is alive and we just confirmed it's ready.
	if err := cmd.Process.Release(); err != nil {
		slog.Warn("memory server: process release", "err", err)
	}

	slog.Info("memory server: started", "port", port, "pid", cmd.Process.Pid)
	return port, nil
}

// Stop kills the server process and cleans up PID and port files. Also
// closes the demarkus-mcp subprocess if one was started via NewStore — we
// shut down the MCP client before the server it depends on.
func (m *Manager) Stop() error {
	if m.mcpClient != nil {
		if err := m.mcpClient.Close(); err != nil {
			slog.Debug("memory: mcp client close", "err", err)
		}
		m.mcpClient = nil
	}

	pid := m.processPID()
	if pid == 0 {
		return nil
	}

	if err := terminatePID(pid, 5*time.Second); err != nil {
		slog.Debug("memory server: terminate", "pid", pid, "err", err)
	}
	m.cleanupFiles()
	m.process = nil
	m.port = 0
	return nil
}

// terminatePID sends SIGTERM to pid, polls up to graceful for it to exit,
// then escalates to SIGKILL. Polling via signal 0 is used because the
// target is not a child process (servers are Release()'d post-spawn, and
// orphan sweeps discover PIDs via ps), so Wait() would return immediately
// without observing exit. Returns an error only if the PID was already
// gone when the first signal was sent.
func terminatePID(pid int, graceful time.Duration) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM %d: %w", pid, err)
	}
	deadline := time.Now().Add(graceful)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			slog.Info("memory server: terminated gracefully", "pid", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		slog.Debug("memory server: SIGKILL failed (already dead)", "pid", pid, "err", err)
	} else {
		time.Sleep(200 * time.Millisecond)
	}
	slog.Info("memory server: terminated (killed)", "pid", pid)
	return nil
}

// reapOrphans kills any demarkus-server processes bound to this manager's
// content directory, except sparePID. sparePID=0 means kill all matches.
//
// This exists because demarkus-server, combined with SysProcAttr.Setpgid
// and Process.Release() at launch, deliberately survives an unclean junto
// exit. That only works if the next junto launch reliably adopts (via PID
// file) or reaps (via this sweep) the survivor. The PID file is fragile:
// reuseExisting deletes it whenever the live PID can't be probed, so a
// subsequent launch sees no PID reference and would otherwise spawn a
// fresh server alongside the orphan, accumulating zombies over time.
//
// Best-effort: failures to list processes or terminate individual PIDs
// are logged but do not block Start. Only Unix platforms are supported
// (matches the rest of this package, which uses POSIX signals).
func (m *Manager) reapOrphans(sparePID int) {
	pids, err := findDemarkusServerPIDs(m.contentDir)
	if err != nil {
		slog.Debug("memory server: orphan scan failed", "err", err)
		return
	}
	for _, pid := range pids {
		if pid == sparePID {
			continue
		}
		slog.Warn("memory server: reaping orphan", "pid", pid, "contentDir", m.contentDir)
		if err := terminatePID(pid, 5*time.Second); err != nil {
			slog.Warn("memory server: reap failed", "pid", pid, "err", err)
		}
	}
}

// findDemarkusServerPIDs returns PIDs of demarkus-server processes whose
// `-root <contentDir>` argument matches the given path. Uses ps since
// junto already depends on POSIX process semantics.
func findDemarkusServerPIDs(contentDir string) ([]int, error) {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// First field is PID; rest is the command line (possibly with
		// spaces, but argv tokens are space-separated).
		space := strings.IndexByte(line, ' ')
		if space < 0 {
			continue
		}
		pidStr := line[:space]
		cmdline := strings.TrimSpace(line[space+1:])
		if !demarkusServerMatches(cmdline, contentDir) {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// demarkusServerMatches reports whether the argv-string represents a
// demarkus-server invocation with `-root <contentDir>`. The executable
// path may include any prefix (absolute or relative) but must end in the
// binary name demarkus-server. The `-root` argument must match contentDir
// exactly — substring matches are rejected to avoid confusing sibling
// projects whose paths share a prefix.
func demarkusServerMatches(cmdline, contentDir string) bool {
	tokens := strings.Fields(cmdline)
	if len(tokens) == 0 {
		return false
	}
	if filepath.Base(tokens[0]) != "demarkus-server" {
		return false
	}
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i] == "-root" && tokens[i+1] == contentDir {
			return true
		}
	}
	return false
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
// Both the raw token file (.memory-token) and the hashed token registry
// (.memory-tokens.toml) must exist — if either is missing, the pair is
// regenerated. This prevents a state where the raw token is cached but
// the server has no registry to validate it against.
func (m *Manager) EnsureToken() (string, error) {
	// Only reuse cached token if both files are present.
	_, rawErr := os.Stat(m.tokenFile)
	_, regErr := os.Stat(m.tokensFile)
	if rawErr == nil && regErr == nil {
		if data, err := os.ReadFile(m.tokenFile); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				return token, nil
			}
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

	// Token output is tiny (hex string) — cap at 1KB to guard against a noisy binary.
	outBuf := &limitedBuffer{limit: 1024}
	cmd.Stdout = outBuf
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("memory server: generate token: %w", err)
	}
	if outBuf.overflow {
		return "", errors.New("memory server: token output exceeded 1KB")
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

// mcpInitTimeout bounds the demarkus-mcp handshake. Large enough that a
// slow-to-start subprocess doesn't spuriously fail, small enough that we
// don't block startup indefinitely if the binary is broken.
const mcpInitTimeout = 10 * time.Second

// NewStore spawns a demarkus-mcp subprocess pointed at this manager's server
// and returns a memory.Store backed by the MCP adapter. Must be called after
// Start and EnsureToken. The subprocess is owned by the Manager — Stop closes
// it. Any partially-started subprocess is torn down on error.
func (m *Manager) NewStore(token string) (memory.Store, error) {
	// Guard against double-call without an intervening Stop(). Stop() clears
	// mcpClient, so this only fires if a caller accidentally re-invokes
	// NewStore on the same live Manager.
	if m.mcpClient != nil {
		if err := m.mcpClient.Close(); err != nil {
			slog.Debug("memory: closing stale mcp client before rebuild", "err", err)
		}
		m.mcpClient = nil
	}
	binPath := filepath.Join(m.binDir, "demarkus-mcp")
	args := []string{
		"-host", m.Address(),
		"-token", token,
		"-insecure",
		"-no-cache",
	}
	client, err := mcp.NewStdioClient(binPath, args, nil)
	if err != nil {
		return nil, fmt.Errorf("memory: spawn demarkus-mcp: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpInitTimeout)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close() // best-effort cleanup; primary error is init
		return nil, fmt.Errorf("memory: demarkus-mcp initialize: %w", err)
	}
	m.mcpClient = client
	return mcpadapter.New(client), nil
}

// errNoExistingServer indicates no running server was found (PID file absent,
// corrupt, or process dead). Safe to launch a new one.
var errNoExistingServer = errors.New("no existing server")

// reuseExisting checks for a running server via the PID file.
// Returns the port if the server is alive and responding.
// Returns errNoExistingServer when it's safe to start fresh.
// Returns a different error when a live process exists but can't be reused
// (port file broken, probe failed) — callers must NOT start a second server.
func (m *Manager) reuseExisting() (int, error) {
	pidData, err := os.ReadFile(m.pidFile)
	if err != nil {
		return 0, fmt.Errorf("%w: no PID file", errNoExistingServer)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		m.cleanupFiles()
		return 0, fmt.Errorf("%w: corrupt PID file", errNoExistingServer)
	}

	// Check if process is alive (signal 0 doesn't kill, just checks).
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errNoExistingServer, err)
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is dead — clean up stale files.
		m.cleanupFiles()
		return 0, fmt.Errorf("%w: stale PID %d", errNoExistingServer, pid)
	}

	// Process is alive from here — errors below are NOT safe to fall through.
	portData, err := os.ReadFile(m.portFile)
	if err != nil {
		return 0, fmt.Errorf("live PID %d but port file unreadable: %w", pid, err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(portData)))
	if err != nil {
		return 0, fmt.Errorf("live PID %d but port file corrupt: %w", pid, err)
	}

	// Probe the port to verify this is actually a demarkus server and not
	// a recycled PID. Without this, a dead server whose PID was reused by
	// an unrelated process would be silently adopted, and Stop() would
	// later signal the wrong process.
	clientBin := filepath.Join(m.binDir, "demarkus")
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()
	probe := exec.CommandContext(probeCtx, clientBin, "-insecure", "-no-cache", fmt.Sprintf("mark://localhost:%d/", port))
	probe.Stdout = io.Discard
	probe.Stderr = io.Discard
	if err := probe.Run(); err != nil {
		// Live PID, unresponsive QUIC endpoint — the server is wedged.
		// Terminate it here so Start() can launch a healthy replacement;
		// otherwise cleanupFiles() below would drop the PID reference and
		// next junto launch would orphan this process permanently.
		slog.Warn("memory server: adopting PID probe failed; terminating", "pid", pid, "port", port, "err", err)
		if termErr := terminatePID(pid, 5*time.Second); termErr != nil {
			slog.Warn("memory server: terminate unresponsive server failed", "pid", pid, "err", termErr)
		}
		m.cleanupFiles()
		return 0, fmt.Errorf("%w: PID %d alive but port %d not responding", errNoExistingServer, pid, port)
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
		probeCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		cmd := exec.CommandContext(probeCtx, clientBin, "-insecure", "-no-cache", url)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		err := cmd.Run()
		cancel()
		if err == nil {
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

// limitedBuffer captures output up to a limit, silently discarding excess.
type limitedBuffer struct {
	buf      strings.Builder
	written  int
	limit    int
	overflow bool
}

// Write implements io.Writer.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	remaining := b.limit - b.written
	if remaining <= 0 {
		b.overflow = true
		return origLen, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	n, err := b.buf.Write(p)
	b.written += n
	return origLen, err
}

// String returns the captured output.
func (b *limitedBuffer) String() string { return b.buf.String() }

// freePort finds an available port by binding to :0 and reading the assigned port.
func freePort() (int, error) {
	l, err := net.ListenPacket("udp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }() // best-effort — port already captured
	return l.LocalAddr().(*net.UDPAddr).Port, nil
}
