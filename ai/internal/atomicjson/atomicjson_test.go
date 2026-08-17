package atomicjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_RoundTripAndNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "f.json")
	in := map[string]int{"a": 1}
	if err := Write(path, in, 0o700, 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var out map[string]int
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out["a"] != 1 {
		t.Errorf("round trip = %v; want a=1", out)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file perm = %o; want 600", info.Mode().Perm())
	}
}

func TestWrite_MarshalErrorLeavesNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := Write(path, make(chan int), 0o700, 0o600); err == nil {
		t.Fatal("Write of unmarshalable value succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file created despite marshal error: %v", err)
	}
}
