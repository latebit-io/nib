package filecap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead_UnderCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path, 1<<20, "test")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want hello", data)
	}
}

func TestRead_OverCapRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path, 4, "skill")
	if err == nil {
		t.Fatal("expected an error for a file over the cap")
	}
	// The kind label appears in the message so the caller's diagnostics
	// say which loader rejected it.
	if !strings.Contains(err.Error(), "skill cap") {
		t.Errorf("error %q should mention the kind label", err)
	}
}

func TestRead_ExactlyAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact.txt")
	if err := os.WriteFile(path, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, 4, "test"); err != nil {
		t.Fatalf("a file exactly at the cap should be accepted, got %v", err)
	}
}

func TestRead_MissingFile(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "nope.txt"), 1<<20, "test"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
