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

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/memory"
)

// StoreFactory wraps a connected demarkus-mcp client into a memory.Store.
// Composition roots inject the concrete adapter (e.g. mcpadapter.New) so
// the server package depends only on the [memory.Store] port and the MCP
// client transport, not on any specific store implementation.
type StoreFactory func(*mcp.Client) memory.Store

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
	lockPath    string // .project/.memory-lock (flock target)

	port    int
	process *os.Process

	// mcpClient is the demarkus-mcp subprocess created by NewStore. Closed by
	// Stop so the subprocess doesn't outlive the server it talks to.
	mcpClient *mcp.Client

	// cmd is the *exec.Cmd for a server we spawned this session. nil when
	// the server was adopted via reuseExisting (not our child). A background
	// goroutine calls cmd.Wait() to reap on exit; waitDone is closed when
	// Wait returns, giving Stop() an accurate exit signal without the
	// signal(0) zombie-polling pitfall.
	cmd      *exec.Cmd
	waitDone chan struct{}

	// lockFile holds the exclusive flock on lockPath while a session is
	// active. Prevents two concurrent host instances from sharing — and
	// clobbering — the same memory server. Released in Stop() or when the
	// process exits (kernel releases the fd).
	lockFile *os.File
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
		lockPath:    filepath.Join(dot, ".memory-lock"),
	}
}

// ErrInstanceAlreadyRunning is returned when another host instance is
// actively managing this project's memory server. Concurrent instances
// would race on PID/port files and (worse) kill each other's servers at
// teardown — the flock prevents that.
var ErrInstanceAlreadyRunning = fmt.Errorf("another %s instance is managing this project", brand.Name)

// Start launches the demarkus-server. Returns the port it's listening on.
// If a server is already running (detected via PID file), reuses it.
// Any orphaned demarkus-server processes bound to this project's content
// directory are reaped before returning — unresponsive servers from
// earlier sessions (e.g. the host crashed before Stop() ran, or the PID file
// was lost) would otherwise accumulate alongside the live one.
func (m *Manager) Start() (int, error) {
	// Ensure content directory exists.
	if err := os.MkdirAll(m.contentDir, 0755); err != nil {
		return 0, fmt.Errorf("memory server: create content dir: %w", err)
	}

	// Acquire the single-instance lock before touching any state files. A
	// second host instance that reached this point concurrently would call
	// reuseExisting, adopt this instance's server, and then kill it at its
	// own teardown — leaving this instance's MCP client talking to a dead
	// port for the rest of its session (the exact timeout-storm we've seen
	// in the field). Crash-safe: kernel releases the flock on exit.
	if err := m.acquireLock(); err != nil {
		return 0, err
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
		m.releaseLock()
		return 0, fmt.Errorf("memory server: existing server detected but unusable: %w", reuseErr)
	}

	// No existing server — reap any untracked orphans before launching
	// fresh. Without this, a prior session that lost its PID file would
	// leave a zombie that accumulates indefinitely.
	m.reapOrphans(0)

	return m.startFresh()
}

// startFresh launches a new demarkus-server. Precondition: the single-
// instance lock is already held. Releases the lock on any failure path
// so callers don't need to track partial state.
func (m *Manager) startFresh() (int, error) {
	port, err := freePort()
	if err != nil {
		m.releaseLock()
		return 0, fmt.Errorf("memory server: find free port: %w", err)
	}

	// Tokens file must exist — EnsureToken must be called before Start.
	if _, err := os.Stat(m.tokensFile); err != nil {
		m.releaseLock()
		return 0, fmt.Errorf("memory server: tokens file missing (call EnsureToken first): %w", err)
	}

	serverBin := filepath.Join(m.binDir, "demarkus-server")
	cmd := exec.Command(serverBin,
		"-root", m.contentDir,
		"-port", strconv.Itoa(port),
		"-tokens", m.tokensFile,
	)
	// Detach from parent process group so the server survives if the host crashes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		m.releaseLock()
		return 0, fmt.Errorf("memory server: start: %w", err)
	}

	// Register owned-child bookkeeping immediately so every post-Start
	// failure path routes through m.Stop() → terminateOwnedChild, which
	// uses waitDone to reap. Setting m.cmd/m.waitDone later (after
	// waitReady) would leave an early-failure window where Stop() falls
	// back to terminatePID — that path signals but cannot reap, so a
	// server exiting during startup would linger as a zombie until the host
	// itself exits.
	m.cmd = cmd
	m.waitDone = make(chan struct{})
	go func() {
		defer close(m.waitDone)
		if err := cmd.Wait(); err != nil {
			slog.Debug("memory server: wait returned", "pid", cmd.Process.Pid, "err", err)
		}
	}()
	m.process = cmd.Process
	m.port = port

	if err := m.writePIDPortFiles(cmd.Process.Pid, port); err != nil {
		if stopErr := m.Stop(); stopErr != nil {
			slog.Warn("memory server: stop after state-file write failure", "stopErr", stopErr)
		}
		return 0, err
	}

	// Wait for server to become ready. On failure Stop() handles all
	// cleanup including lock release and zombie-free child reaping.
	if err := m.waitReady(port); err != nil {
		if stopErr := m.Stop(); stopErr != nil {
			slog.Warn("memory server: stop after readiness failure", "stopErr", stopErr)
		}
		return 0, fmt.Errorf("memory server: not ready: %w", err)
	}

	slog.Info("memory server: started", "port", port, "pid", cmd.Process.Pid)
	return port, nil
}

// writePIDPortFiles persists the spawned server's PID and port for
// adoption by future host launches. Pure I/O — cleanup on failure is
// the caller's responsibility (via [Manager.Stop]), so both
// file-write failures and readiness failures share one cleanup path.
func (m *Manager) writePIDPortFiles(pid, port int) error {
	if err := os.WriteFile(m.pidFile, []byte(strconv.Itoa(pid)), 0600); err != nil {
		return fmt.Errorf("memory server: write PID file: %w", err)
	}
	if err := os.WriteFile(m.portFile, []byte(strconv.Itoa(port)), 0600); err != nil {
		return fmt.Errorf("memory server: write port file: %w", err)
	}
	return nil
}

// acquireLock takes an exclusive, non-blocking flock on lockPath. Fails
// fast with [ErrInstanceAlreadyRunning] when another host instance holds it. The
// OS releases the flock when the process exits, so crashes don't wedge
// the next launch.
func (m *Manager) acquireLock() error {
	if err := os.MkdirAll(filepath.Dir(m.lockPath), 0755); err != nil {
		return fmt.Errorf("memory server: lock dir: %w", err)
	}
	f, err := os.OpenFile(m.lockPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("memory server: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Close error is intentionally discarded: flock already failed,
		// so the primary error takes precedence. Close on an un-flocked
		// regular file can only fail in ways (EBADF) that would indicate
		// a Go runtime bug, not something the caller can act on.
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("%w (lock: %s)", ErrInstanceAlreadyRunning, m.lockPath)
		}
		return fmt.Errorf("memory server: flock: %w", err)
	}
	m.lockFile = f
	return nil
}

// releaseLock unlocks and closes the flock file. Idempotent.
func (m *Manager) releaseLock() {
	if m.lockFile == nil {
		return
	}
	if err := syscall.Flock(int(m.lockFile.Fd()), syscall.LOCK_UN); err != nil {
		slog.Debug("memory server: flock unlock", "err", err)
	}
	if err := m.lockFile.Close(); err != nil {
		slog.Debug("memory server: lock file close", "err", err)
	}
	m.lockFile = nil
}

// Stop kills the server process and cleans up PID and port files. Also
// closes the demarkus-mcp subprocess if one was started via NewStore — we
// shut down the MCP client before the server it depends on. Releases the
// single-instance lock on the way out.
func (m *Manager) Stop() error {
	if m.mcpClient != nil {
		if err := m.mcpClient.Close(); err != nil {
			slog.Debug("memory: mcp client close", "err", err)
		}
		m.mcpClient = nil
	}

	pid := m.processPID()
	if pid == 0 {
		m.releaseLock()
		return nil
	}

	// Spawned child vs. adopted non-child need different exit detection.
	// Our own child: use waitDone (the Wait goroutine closes it on reap),
	//   which cannot be fooled by zombie state.
	// Adopted (reuseExisting) or sweep target: fall back to signal(0)
	//   polling since we have no Wait() channel for non-children.
	if m.cmd != nil && m.waitDone != nil {
		if err := terminateOwnedChild(m.cmd.Process, m.waitDone, 5*time.Second); err != nil {
			slog.Debug("memory server: terminate child", "pid", pid, "err", err)
		}
	} else {
		if err := terminatePID(pid, 5*time.Second); err != nil {
			slog.Debug("memory server: terminate", "pid", pid, "err", err)
		}
	}
	m.cleanupFiles()
	m.process = nil
	m.port = 0
	m.cmd = nil
	m.waitDone = nil
	m.releaseLock()
	return nil
}

// terminatePID sends SIGTERM to a non-child pid, polls up to graceful for
// it to exit, then escalates to SIGKILL. Used when we do not own the
// process (reuseExisting adoption and orphan sweep) and therefore have
// no Wait channel. Relies on init reaping reparented children for
// signal(0) → ESRCH to work correctly. Returns an error only if the PID
// was already gone when the first signal was sent.
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

// terminateOwnedChild terminates a process we spawned in this session.
// Unlike [terminatePID], exit is observed via a caller-provided done
// channel (closed by the Wait goroutine), which is accurate even while
// the OS briefly leaves the PID as a zombie between exit and reap.
func terminateOwnedChild(proc *os.Process, done <-chan struct{}, graceful time.Duration) error {
	if proc == nil {
		return fmt.Errorf("nil process")
	}
	pid := proc.Pid
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM %d: %w", pid, err)
	}
	select {
	case <-done:
		slog.Info("memory server: terminated gracefully", "pid", pid)
		return nil
	case <-time.After(graceful):
		// Still alive after the grace window — escalate.
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		slog.Debug("memory server: SIGKILL failed (already dead)", "pid", pid, "err", err)
	}
	// Wait for the kernel to reap via our Wait goroutine. SIGKILL is
	// unblockable; the bound is just belt-and-braces against a stuck
	// reaper (e.g. process stuck in uninterruptible sleep).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn("memory server: Wait did not return after SIGKILL", "pid", pid)
	}
	slog.Info("memory server: terminated (killed)", "pid", pid)
	return nil
}

// reapOrphans kills any demarkus-server processes bound to this manager's
// content directory, except sparePID. sparePID=0 means kill all matches.
//
// This exists because demarkus-server, combined with SysProcAttr.Setpgid
// and Process.Release() at launch, deliberately survives an unclean host
// exit. That only works if the next host launch reliably adopts (via PID
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

// pidOwnsDemarkusServer reports whether pid is currently a demarkus-
// server process for this manager's content directory. Used before
// terminating an adopted PID whose port probe failed — signal(0) alone
// cannot distinguish between "our server is wedged" and "our PID got
// recycled by an unrelated process after the original server exited."
// Without this check, blind termination on probe failure could SIGKILL
// the user's unrelated process.
//
// Scan errors are logged and treated as "not ours" — failing closed on
// a best-effort identity check is the safer default. The caller still
// cleans up stale state files and proceeds to launch a fresh server.
func (m *Manager) pidOwnsDemarkusServer(pid int) bool {
	matches, err := findDemarkusServerPIDs(m.contentDir)
	if err != nil {
		slog.Warn("memory server: PID ownership check failed", "pid", pid, "err", err)
		return false
	}
	for _, match := range matches {
		if match == pid {
			return true
		}
	}
	return false
}

// findDemarkusServerPIDs returns PIDs of demarkus-server processes whose
// `-root <contentDir>` argument matches the given path. Uses ps since
// the host already depends on POSIX process semantics. The `-ww` flag
// disables terminal-width truncation; without it, long project paths
// get clipped and matches silently fail.
func findDemarkusServerPIDs(contentDir string) ([]int, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
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
// binary name demarkus-server.
//
// Argv boundaries are lost in ps output — spaces in both the executable
// path (project under a path like "/tmp/foo bar/...") and flag values
// collapse with spaces between tokens. Reconstruction rejoins tokens:
// argv[0] is everything up to the first flag-looking token (leading
// `-`); each flag value is everything after it up to the next flag or
// end of line. Handles realistic paths with single spaces; does not
// round-trip paths whose components begin with `-` or contain multiple
// consecutive whitespace characters (both atypical for project dirs).
// `-root` is matched exactly, not as a substring, so sibling projects
// whose paths share a prefix cannot collide.
func demarkusServerMatches(cmdline, contentDir string) bool {
	tokens := strings.Fields(cmdline)
	if len(tokens) == 0 {
		return false
	}

	// Reconstruct argv[0] before checking the binary name. Without this,
	// a binary path containing a space (e.g. "/tmp/foo bar/bin/demarkus-
	// server") tokenises such that tokens[0] is just the prefix
	// ("/tmp/foo"), causing filepath.Base to return the wrong value and
	// the match to silently fail — missing live servers for spaced
	// project roots.
	firstFlag := len(tokens)
	for i, tok := range tokens {
		if strings.HasPrefix(tok, "-") {
			firstFlag = i
			break
		}
	}
	if firstFlag == 0 {
		return false
	}
	if filepath.Base(strings.Join(tokens[:firstFlag], " ")) != "demarkus-server" {
		return false
	}

	for i := firstFlag; i < len(tokens)-1; i++ {
		if tokens[i] != "-root" {
			continue
		}
		root := tokens[i+1]
		for j := i + 2; j < len(tokens); j++ {
			if strings.HasPrefix(tokens[j], "-") {
				break
			}
			root += " " + tokens[j]
		}
		if root == contentDir {
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
// ctx bounds all install-time network I/O so a stalled download can be
// cancelled.
func (m *Manager) EnsureBinaries(ctx context.Context) error {
	for _, name := range strings.Split(requiredBins, ",") {
		path := filepath.Join(m.binDir, name)
		if _, err := os.Stat(path); err != nil {
			versionFile := filepath.Join(m.projectRoot, ".project", ".memory-version")
			return install(ctx, m.binDir, versionFile)
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
		"-label", brand.ProcessLabel,
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

// NewStore spawns a demarkus-mcp subprocess pointed at this manager's
// server and wraps the resulting client via the supplied factory. Must be
// called after Start and EnsureToken. The subprocess is owned by the
// Manager — Stop closes it. Any partially-started subprocess is torn
// down on error. A nil factory is treated as a programmer error.
func (m *Manager) NewStore(token string, factory StoreFactory) (memory.Store, error) {
	if factory == nil {
		return nil, errors.New("memory: nil StoreFactory")
	}
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
	return factory(client), nil
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
		// Probe failed. Two distinct scenarios must be distinguished —
		// conflating them risks killing an unrelated process:
		//   a) demarkus-server is wedged (original bug we're guarding).
		//      Terminate it so Start() can launch a healthy replacement;
		//      without this, cleanupFiles() below drops the PID reference
		//      and the next host launch orphans it permanently.
		//   b) PID was recycled by an unrelated process after the prior
		//      demarkus-server exited (e.g. the user's editor now holds
		//      this PID). signal(0) earlier proved "a process exists";
		//      it did NOT prove "that process is our server." Killing
		//      blindly here could SIGKILL the user's unrelated work.
		// Verify identity via ps before terminating.
		if m.pidOwnsDemarkusServer(pid) {
			slog.Warn("memory server: adopting PID probe failed; terminating", "pid", pid, "port", port, "err", err)
			if termErr := terminatePID(pid, 5*time.Second); termErr != nil {
				slog.Warn("memory server: terminate unresponsive server failed", "pid", pid, "err", termErr)
			}
		} else {
			slog.Info("memory server: PID recycled by unrelated process; skipping terminate", "pid", pid, "port", port)
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

// cleanupFiles removes PID and port files. Remove errors are
// intentionally discarded: the common reason either Remove fails is
// ENOENT (file already gone), which is the desired end state. Any
// other error (permission, filesystem) would leave a stale state file
// that [reuseExisting] already tolerates — it validates liveness
// before trusting the PID.
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
