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

func TestCheckStatus(t *testing.T) {
	baseErr := errors.New("exit status 1")

	tests := []struct {
		name    string
		stderr  string
		cmdErr  error
		wantErr error // nil means expect no error
	}{
		{
			name:   "ok status returns nil",
			stderr: "[ok] version=1 modified=2026-04-01T00:00:00Z",
		},
		{
			name:    "not-found maps to ErrNotFound",
			stderr:  "[not-found] version=0",
			wantErr: memory.ErrNotFound,
		},
		{
			name:    "conflict maps to ErrConflict",
			stderr:  "[conflict] version=3",
			wantErr: memory.ErrConflict,
		},
		{
			name:    "unauthorized maps to ErrAuth",
			stderr:  "[unauthorized]",
			wantErr: memory.ErrAuth,
		},
		{
			name:    "error maps to ErrServer",
			stderr:  "[error] something broke",
			wantErr: memory.ErrServer,
		},
		{
			name:    "server-error maps to ErrServer",
			stderr:  "[server-error] disk full",
			wantErr: memory.ErrServer,
		},
		{
			name:    "unknown status with cmdErr wraps it",
			stderr:  "connection refused",
			cmdErr:  baseErr,
			wantErr: baseErr,
		},
		{
			name:    "empty stderr with cmdErr wraps it",
			stderr:  "",
			cmdErr:  baseErr,
			wantErr: baseErr,
		},
		{
			name:   "no status no cmdErr returns nil",
			stderr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkStatus(tt.stderr, tt.cmdErr)
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
