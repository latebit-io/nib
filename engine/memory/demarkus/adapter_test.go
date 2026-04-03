package demarkus

import (
	"errors"
	"testing"

	"github.com/latebit-io/junto/engine/memory"
)

func TestParseStatusLine(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		wantMeta metadata
	}{
		{
			name:   "ok with version and modified",
			stderr: "[ok] version=3 modified=2026-04-01T12:00:00Z etag=abc123\n",
			wantMeta: metadata{
				status:   "ok",
				version:  3,
				modified: "2026-04-01T12:00:00Z",
			},
		},
		{
			name:     "not-found status",
			stderr:   "[not-found]\n",
			wantMeta: metadata{status: "not-found"},
		},
		{
			name:   "conflict status",
			stderr: "[conflict] version=5\n",
			wantMeta: metadata{
				status:  "conflict",
				version: 5,
			},
		},
		{
			name:     "empty stderr",
			stderr:   "",
			wantMeta: metadata{},
		},
		{
			name:     "no brackets",
			stderr:   "some random error\n",
			wantMeta: metadata{},
		},
		{
			name:   "cached flag ignored",
			stderr: "[ok] version=1 modified=2026-01-01T00:00:00Z (cached)\n",
			wantMeta: metadata{
				status:   "ok",
				version:  1,
				modified: "2026-01-01T00:00:00Z",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStatusLine(tt.stderr)
			if got.status != tt.wantMeta.status {
				t.Errorf("status: got %q, want %q", got.status, tt.wantMeta.status)
			}
			if got.version != tt.wantMeta.version {
				t.Errorf("version: got %d, want %d", got.version, tt.wantMeta.version)
			}
			if got.modified != tt.wantMeta.modified {
				t.Errorf("modified: got %q, want %q", got.modified, tt.wantMeta.modified)
			}
		})
	}
}

var (
	errTestExit      = errors.New("exit status 1")
	checkStatusTests = []struct {
		name       string
		stderr     string
		cmdErr     error
		wantErr    error
		wantNonNil bool
	}{
		{"ok status returns nil", "[ok] version=1 modified=2026-04-01T00:00:00Z", nil, nil, false},
		{"not-found", "[not-found] version=0", nil, memory.ErrNotFound, false},
		{"conflict", "[conflict] version=3", nil, memory.ErrConflict, false},
		{"unauthorized", "[unauthorized]", nil, memory.ErrAuth, false},
		{"error", "[error] something broke", nil, memory.ErrServer, false},
		{"server-error", "[server-error] disk full", nil, memory.ErrServer, false},
		{"unknown status with cmdErr", "connection refused", errTestExit, errTestExit, false},
		{"empty stderr with cmdErr", "", errTestExit, errTestExit, false},
		{"no status line without cmdErr", "", nil, nil, true},
		{"ok with cmdErr", "[ok] version=1", errTestExit, nil, true},
	}
)

func TestCheckStatus(t *testing.T) {
	for _, tt := range checkStatusTests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkStatus(tt.stderr, tt.cmdErr)
			if tt.wantNonNil {
				if got == nil {
					t.Error("expected an error, got nil")
				}
				return
			}
			if tt.wantErr == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if !errors.Is(got, tt.wantErr) {
				t.Errorf("got error %v, want errors.Is(%v)", got, tt.wantErr)
			}
		})
	}
}
