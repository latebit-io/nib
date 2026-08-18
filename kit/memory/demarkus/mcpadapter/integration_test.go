package mcpadapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/contracttest"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/memory/demarkus/mcpadapter"
	"github.com/latebit-io/nib/kit/memory/demarkus/server"
)

// integrationEnvVar opts the network-touching integration tests in.
// They download demarkus binaries, so they never run under plain
// `go test`; set the variable to any non-empty value to enable them.
var integrationEnvVar = brand.EnvPrefix + "INTEGRATION"

// requireIntegration skips the calling test unless integration tests are
// enabled via integrationEnvVar (and not -short).
func requireIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() || os.Getenv(integrationEnvVar) == "" {
		t.Skipf("integration test disabled; set %s=1 (downloads demarkus binaries)", integrationEnvVar)
	}
}

// integrationEnv holds the shared state for integration tests.
type integrationEnv struct {
	store  memory.Store
	binDir string
	mgr    *server.Manager
}

// setupIntegration downloads demarkus, starts the server, and returns a ready-to-use env.
// Skips the calling test unless integration tests are enabled (see requireIntegration).
func setupIntegration(t *testing.T) *integrationEnv {
	t.Helper()
	requireIntegration(t)

	root := t.TempDir()
	mgr := server.New(root)

	t.Log("installing demarkus binaries...")
	if err := mgr.EnsureBinaries(t.Context()); err != nil {
		t.Fatalf("EnsureBinaries: %v", err)
	}

	token, err := mgr.EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}

	port, err := mgr.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := mgr.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	t.Logf("server running on port %d", port)

	binDir := filepath.Join(root, ".project", "bin")
	store, err := mgr.NewStore(token, func(c *mcp.Client) memory.Store { return mcpadapter.New(c) })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	return &integrationEnv{store: store, binDir: binDir, mgr: mgr}
}

// newAuxStore spawns a separate demarkus-mcp subprocess with a specific
// token (empty for unauthenticated). The returned store is closed via
// t.Cleanup. Used when a test needs a second adapter bound to different
// credentials than the env's primary store.
func newAuxStore(t *testing.T, binDir, serverAddress, token string) memory.Store {
	t.Helper()
	binPath := filepath.Join(binDir, "demarkus-mcp")
	args := []string{"-host", serverAddress, "-token", token, "-insecure", "-no-cache"}
	client, err := mcp.NewStdioClient(binPath, args, nil)
	if err != nil {
		t.Fatalf("spawn demarkus-mcp: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		t.Fatalf("initialize demarkus-mcp: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return mcpadapter.New(client)
}

// TestIntegrationInstall verifies binary download, version pinning, and idempotency.
func TestIntegrationInstall(t *testing.T) {
	requireIntegration(t)

	root := t.TempDir()
	mgr := server.New(root)

	if err := mgr.EnsureBinaries(t.Context()); err != nil {
		t.Fatalf("EnsureBinaries: %v", err)
	}

	binDir := filepath.Join(root, ".project", "bin")
	for _, name := range []string{"demarkus-server", "demarkus-token", "demarkus", "demarkus-mcp"} {
		info, err := os.Stat(filepath.Join(binDir, name))
		if err != nil {
			t.Fatalf("binary %s not found: %v", name, err)
		}
		if info.Mode()&0111 == 0 {
			t.Errorf("binary %s is not executable", name)
		}
	}

	versionData, err := os.ReadFile(filepath.Join(root, ".project", ".memory-version"))
	if err != nil {
		t.Fatalf("read version file: %v", err)
	}
	if strings.TrimSpace(string(versionData)) == "" {
		t.Fatal("version file is empty")
	}

	// Second call is a no-op.
	if err := mgr.EnsureBinaries(t.Context()); err != nil {
		t.Fatalf("second EnsureBinaries: %v", err)
	}
}

// TestIntegrationTokenIdempotent verifies token bootstrap returns the same token on re-run.
func TestIntegrationTokenIdempotent(t *testing.T) {
	requireIntegration(t)

	root := t.TempDir()
	mgr := server.New(root)
	if err := mgr.EnsureBinaries(t.Context()); err != nil {
		t.Fatalf("EnsureBinaries: %v", err)
	}

	token1, err := mgr.EnsureToken()
	if err != nil {
		t.Fatalf("first EnsureToken: %v", err)
	}
	token2, err := mgr.EnsureToken()
	if err != nil {
		t.Fatalf("second EnsureToken: %v", err)
	}
	if token1 != token2 {
		t.Error("token should be stable across calls")
	}
}

// TestIntegrationCRUD exercises create, read, update, append against a live server.
func TestIntegrationCRUD(t *testing.T) {
	env := setupIntegration(t)

	// Create.
	doc, err := env.store.Publish(context.Background(), "/test.md", "# Hello World", 0)
	if err != nil {
		t.Fatalf("Publish create: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("create version: got %d, want 1", doc.Version)
	}

	// Read.
	doc, err = env.store.Fetch(context.Background(), "/test.md")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.TrimSpace(doc.Body) != "# Hello World" {
		t.Errorf("body: got %q", doc.Body)
	}

	// Update.
	doc, err = env.store.Publish(context.Background(), "/test.md", "# Updated", 1)
	if err != nil {
		t.Fatalf("Publish update: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("update version: got %d, want 2", doc.Version)
	}

	// Append.
	doc, err = env.store.Append(context.Background(), "/test.md", "\n## Section 2\n", 2)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if doc.Version != 3 {
		t.Errorf("append version: got %d, want 3", doc.Version)
	}
	fetched, err := env.store.Fetch(context.Background(), "/test.md")
	if err != nil {
		t.Fatalf("Fetch after append: %v", err)
	}
	if !strings.Contains(fetched.Body, "Section 2") {
		t.Error("appended content not found")
	}
}

// TestIntegrationConflict verifies optimistic concurrency.
func TestIntegrationConflict(t *testing.T) {
	env := setupIntegration(t)

	if _, err := env.store.Publish(context.Background(), "/conflict.md", "v1", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := env.store.Publish(context.Background(), "/conflict.md", "v2", 1); err != nil {
		t.Fatalf("update to v2: %v", err)
	}

	// Stale update with expected_version=1 (current is 2).
	_, err := env.store.Publish(context.Background(), "/conflict.md", "stale", 1)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !errors.Is(err, memory.ErrConflict) {
		t.Errorf("expected ErrConflict, got: %v", err)
	}
}

// TestIntegrationNotFound verifies fetch of a nonexistent document.
func TestIntegrationNotFound(t *testing.T) {
	env := setupIntegration(t)

	_, err := env.store.Fetch(context.Background(), "/nonexistent.md")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestIntegrationList verifies document listing.
func TestIntegrationList(t *testing.T) {
	env := setupIntegration(t)

	if _, err := env.store.Publish(context.Background(), "/alpha.md", "# A", 0); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := env.store.Publish(context.Background(), "/beta.md", "# B", 0); err != nil {
		t.Fatalf("create beta: %v", err)
	}

	paths, err := env.store.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := make(map[string]bool)
	for _, p := range paths {
		found[p] = true
	}
	if !found["alpha.md"] && !found["/alpha.md"] {
		t.Errorf("expected alpha.md in list, got: %v", paths)
	}
	if !found["beta.md"] && !found["/beta.md"] {
		t.Errorf("expected beta.md in list, got: %v", paths)
	}
}

// TestIntegrationStoreContract runs the [contracttest.Store] suite
// against the demarkus mcpadapter. The contract fixture uses unique
// per-subtest paths under /contracttest/ so a shared backing server
// can host the entire suite without inter-test collisions. Skipped in
// -short mode like the rest of the integration suite.
func TestIntegrationStoreContract(t *testing.T) {
	env := setupIntegration(t)
	contracttest.Store(t, func() memory.Store { return env.store })
}

// TestIntegrationUnauthenticated verifies unauthenticated writes are rejected.
func TestIntegrationUnauthenticated(t *testing.T) {
	env := setupIntegration(t)

	// Use an invalid token so the request reaches the server's auth check.
	// An empty token would be rejected client-side by demarkus-mcp before
	// ever hitting the server, which tests a different layer.
	noAuth := newAuxStore(t, env.binDir, env.mgr.Address(), "invalid-token-does-not-exist")
	_, err := noAuth.Publish(context.Background(), "/unauth.md", "# Fail", 0)
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !errors.Is(err, memory.ErrAuth) {
		t.Errorf("expected ErrAuth, got: %v", err)
	}
}
