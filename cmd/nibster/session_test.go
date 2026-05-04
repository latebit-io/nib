package main

import (
	"strings"
	"testing"
	"time"
)

func TestSlugify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "session"},
		{"all-punctuation", "!!!???...", "session"},
		{"ascii", "How can I build an e-mower?", "how-can-i-build-an-e-mower"},
		{"collapses-spaces", "hello   world", "hello-world"},
		{"trims-trailing-dash", "hello!!!", "hello"},
		{"strips-unicode", "héllo wörld", "h-llo-w-rld"},
		{"caps-at-30", strings.Repeat("a", 100), strings.Repeat("a", 30)},
		{"caps-mid-word-then-trims", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa---bbb", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"numbers-allowed", "build123 emower", "build123-emower"},
		{"already-slug", "already-clean", "already-clean"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := slugify(tc.in)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > slugMaxLen {
				t.Errorf("slugify(%q) length = %d, exceeds slugMaxLen=%d", tc.in, len(got), slugMaxLen)
			}
		})
	}
}

func TestNewSessionID_Format(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 4, 14, 30, 45, 0, time.UTC)
	got := newSessionID(now, "Build an e-mower")

	want := "2026-05-04-143045-build-an-e-mower"
	if got != want {
		t.Errorf("newSessionID = %q, want %q", got, want)
	}
}

func TestNewSessionID_NormalizesToUTC(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("LoadLocation: %v", err)
	}
	// 2026-05-04 14:30:45 PDT is 2026-05-04 21:30:45 UTC.
	now := time.Date(2026, 5, 4, 14, 30, 45, 0, loc)
	got := newSessionID(now, "test")

	wantPrefix := "2026-05-04-213045-"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("newSessionID = %q, want prefix %q", got, wantPrefix)
	}
}

func TestNewSessionID_FallbackSlugForBlankMessage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 4, 14, 30, 45, 0, time.UTC)
	got := newSessionID(now, "  \t\n  ")

	if !strings.HasSuffix(got, "-session") {
		t.Errorf("blank message should produce -session suffix, got %q", got)
	}
}
