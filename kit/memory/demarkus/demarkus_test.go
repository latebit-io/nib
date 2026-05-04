package demarkus_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/memory/demarkus"
)

func TestOpen_FailsOnUninstallableRoot(t *testing.T) {
	// /dev/null/nope cannot host a .project directory, so binary
	// install fails fast. Exercises the early-error path before the
	// server starts.
	_, err := demarkus.Open(context.Background(), "/dev/null/nope")
	if err == nil {
		t.Fatal("expected error from Open with invalid root, got nil")
	}
	if !strings.Contains(err.Error(), "demarkus:") {
		t.Errorf("error not wrapped with demarkus: prefix: %v", err)
	}
}

func TestResult_CloseIsIdempotent(t *testing.T) {
	// Construct an empty Result directly — Close should no-op when
	// there's no underlying server. The contract is that Close is
	// safe to call exactly once after a successful Open AND that
	// repeat calls do nothing rather than panic.
	var r *demarkus.Result
	if err := r.Close(); err != nil {
		t.Errorf("Close on nil Result returned error: %v", err)
	}

	r2 := &demarkus.Result{}
	if err := r2.Close(); err != nil {
		t.Errorf("Close on zero Result returned error: %v", err)
	}
	if err := r2.Close(); err != nil {
		t.Errorf("second Close on zero Result returned error: %v", err)
	}
}

func TestOpen_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Bound the entire test — install + start + round-trip — so a
	// stalled download or server start cannot hang CI indefinitely.
	// Real cold-start runs in ~4s; 60s leaves wide margin for slow
	// network without being unbounded.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	t.Log("opening demarkus...")
	root := t.TempDir()
	res, err := demarkus.Open(ctx, root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := res.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if res.Store == nil {
		t.Fatal("Result.Store is nil")
	}
	if res.Port == 0 {
		t.Error("Result.Port is zero")
	}

	// Round-trip a publish/fetch to confirm the wired store actually
	// talks to the server. Serves as the canonical "is the facade
	// composing the pieces correctly?" check. Publish returns
	// metadata; Body is only populated by Fetch. Operations share the
	// outer test deadline so a stuck RPC fails the test rather than
	// blocking forever.
	if _, err := res.Store.Publish(ctx, "/test.md", "hello kit", 0); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	fetched, err := res.Store.Fetch(ctx, "/test.md")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched.Body != "hello kit" {
		t.Errorf("fetched body = %q, want %q", fetched.Body, "hello kit")
	}
}
