package memory_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/memory"
	"github.com/latebit-io/junto/engine/memory/demarkus"
	"github.com/latebit-io/junto/engine/memory/server"
)

// integrationEnv holds the shared state for integration tests.
type integrationEnv struct {
	store  memory.Store
	binDir string
	mgr    *server.Manager
}

// setupIntegration downloads demarkus, starts the server, and returns a ready-to-use env.
// Skips the calling test if -short is set.
func setupIntegration(t *testing.T) *integrationEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	root := t.TempDir()
	mgr := server.New(root)

	t.Log("installing demarkus binaries...")
	if err := mgr.EnsureBinaries(); err != nil {
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
	store := demarkus.New(filepath.Join(binDir, "demarkus"), mgr.Address(), token)

	return &integrationEnv{store: store, binDir: binDir, mgr: mgr}
}

// TestIntegrationInstall verifies binary download, version pinning, and idempotency.
func TestIntegrationInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	root := t.TempDir()
	mgr := server.New(root)

	if err := mgr.EnsureBinaries(); err != nil {
		t.Fatalf("EnsureBinaries: %v", err)
	}

	binDir := filepath.Join(root, ".project", "bin")
	for _, name := range []string{"demarkus-server", "demarkus-token", "demarkus"} {
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
	if err := mgr.EnsureBinaries(); err != nil {
		t.Fatalf("second EnsureBinaries: %v", err)
	}
}

// TestIntegrationTokenIdempotent verifies token bootstrap returns the same token on re-run.
func TestIntegrationTokenIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	root := t.TempDir()
	mgr := server.New(root)
	if err := mgr.EnsureBinaries(); err != nil {
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
	doc, err := env.store.Publish("/test.md", "# Hello World", 0)
	if err != nil {
		t.Fatalf("Publish create: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("create version: got %d, want 1", doc.Version)
	}

	// Read.
	doc, err = env.store.Fetch("/test.md")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.TrimSpace(doc.Body) != "# Hello World" {
		t.Errorf("body: got %q", doc.Body)
	}

	// Update.
	doc, err = env.store.Publish("/test.md", "# Updated", 1)
	if err != nil {
		t.Fatalf("Publish update: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("update version: got %d, want 2", doc.Version)
	}

	// Append.
	doc, err = env.store.Append("/test.md", "\n## Section 2\n", 2)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if doc.Version != 3 {
		t.Errorf("append version: got %d, want 3", doc.Version)
	}
	fetched, err := env.store.Fetch("/test.md")
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

	if _, err := env.store.Publish("/conflict.md", "v1", 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := env.store.Publish("/conflict.md", "v2", 1); err != nil {
		t.Fatalf("update to v2: %v", err)
	}

	// Stale update with expected_version=1 (current is 2).
	_, err := env.store.Publish("/conflict.md", "stale", 1)
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

	_, err := env.store.Fetch("/nonexistent.md")
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

	if _, err := env.store.Publish("/alpha.md", "# A", 0); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := env.store.Publish("/beta.md", "# B", 0); err != nil {
		t.Fatalf("create beta: %v", err)
	}

	paths, err := env.store.List("/")
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

// TestIntegrationUnauthenticated verifies unauthenticated writes are rejected.
func TestIntegrationUnauthenticated(t *testing.T) {
	env := setupIntegration(t)

	noAuth := demarkus.New(filepath.Join(env.binDir, "demarkus"), env.mgr.Address(), "")
	_, err := noAuth.Publish("/unauth.md", "# Fail", 0)
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !errors.Is(err, memory.ErrAuth) {
		t.Errorf("expected ErrAuth, got: %v", err)
	}
}
