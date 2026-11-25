package llm

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// TestHandleErrorStatus_Typed: every non-200 answer is a typed error a
// caller can branch on, and its text still says "status NNN".
func TestHandleErrorStatus_Typed(t *testing.T) {
	t.Parallel()

	failing := errors.New("connection reset")
	tests := []struct {
		name   string
		status int
		header string // Retry-After
		body   io.Reader
		want   StatusError
	}{
		{
			name:   "rate limit with a wait",
			status: http.StatusTooManyRequests,
			header: "7",
			body:   strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`),
			want:   StatusError{Provider: "acme", StatusCode: 429, Code: "rate_limit_error", Detail: "slow down", RetryAfter: 7 * time.Second},
		},
		{
			name:   "provider code takes precedence over its type",
			status: http.StatusTooManyRequests,
			body:   strings.NewReader(`{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"no credit"}}`),
			want:   StatusError{Provider: "acme", StatusCode: 429, Code: "insufficient_quota", Detail: "no credit"},
		},
		{
			name:   "bad request, plain body",
			status: http.StatusBadRequest,
			body:   strings.NewReader("no such model"),
			want:   StatusError{Provider: "acme", StatusCode: 400, Detail: "no such model"},
		},
		{
			name:   "body cannot be read",
			status: http.StatusBadGateway,
			body:   iotest.ErrReader(failing),
			want:   StatusError{Provider: "acme", StatusCode: 502, Detail: "body unreadable: connection reset"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(tc.body)}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}
			err := handleErrorStatus("acme", nil, resp)

			var got *StatusError
			if !errors.As(err, &got) {
				t.Fatalf("error = %v (%T); want a *StatusError", err, err)
			}
			if *got != tc.want {
				t.Errorf("StatusError = %+v; want %+v", *got, tc.want)
			}
			if text := fmt.Sprintf("acme: api error: status %d: %s", tc.status, tc.want.Detail); err.Error() != text {
				t.Errorf("Error() = %q; want %q", err.Error(), text)
			}
		})
	}
}

// TestHandleErrorStatus_UnauthorizedStaysAuthError: a 401 keeps its own
// type, so the credential self-heal is not mistaken for a retryable status.
func TestHandleErrorStatus_UnauthorizedStaysAuthError(t *testing.T) {
	t.Parallel()

	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}
	err := handleErrorStatus("acme", nil, resp)

	var auth *AuthError
	var status *StatusError
	if !errors.As(err, &auth) || errors.As(err, &status) {
		t.Errorf("error = %v (%T); want an *AuthError and no *StatusError", err, err)
	}
}

func TestStatusError_Temporary(t *testing.T) {
	t.Parallel()

	temporary := []int{408, 429, 500, 502, 503, 504, 529}
	for status := 400; status < 600; status++ {
		want := slices.Contains(temporary, status)
		if got := (&StatusError{StatusCode: status}).Temporary(); got != want {
			t.Errorf("status %d: Temporary() = %v; want %v", status, got, want)
		}
	}
	// Out of credit is a 429 that waiting does not fix.
	if (&StatusError{StatusCode: 429, Code: "insufficient_quota"}).Temporary() {
		t.Error("insufficient_quota: Temporary() = true; want false")
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 0},
		{"seconds", "30", 30 * time.Second},
		{"zero seconds", "0", 0},
		{"negative seconds", "-5", 0},
		{"date ahead", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"date in the past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"not a number or a date", "soon", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := retryAfter(tc.header, now); got != tc.want {
				t.Errorf("retryAfter(%q) = %v; want %v", tc.header, got, tc.want)
			}
		})
	}
}

// TestStreamError_Temporary: provider-side failures and rate limits are
// worth another attempt; a request the provider refused is not.
func TestStreamError_Temporary(t *testing.T) {
	for code, want := range map[string]bool{
		"overloaded_error": true, "rate_limit_error": true, "api_error": true, "timeout_error": true,
		"server_error": true, "rate_limit_exceeded": true,
		"529": true, "502": true, "429": true,
		"400": false, "invalid_request_error": false, "insufficient_quota": false, "": false,
	} {
		if got := (&StreamError{Code: code}).Temporary(); got != want {
			t.Errorf("Temporary(%q) = %v; want %v", code, got, want)
		}
	}
}

// TestStreamError_TemporaryByType: a provider that sends a specific code
// with a general type is classified by either; quota is never temporary.
func TestStreamError_TemporaryByType(t *testing.T) {
	tests := []struct {
		reported apiError
		want     bool
	}{
		{apiError{Type: "server_error", Code: "internal_failure"}, true},
		{apiError{Type: "overloaded_error"}, true},
		{apiError{Type: "invalid_request_error", Code: "context_length_exceeded"}, false},
		{apiError{Type: "rate_limit_error", Code: "insufficient_quota"}, false},
		{apiError{Type: "insufficient_quota", Code: "rate_limit_exceeded"}, false},
	}
	for _, tc := range tests {
		if got := tc.reported.streamError().Temporary(); got != tc.want {
			t.Errorf("Temporary(%+v) = %v; want %v", tc.reported, got, tc.want)
		}
	}
}
