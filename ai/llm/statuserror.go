package llm

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// statusOverloaded is the non-standard status Anthropic answers with when
// its capacity is exhausted.
const statusOverloaded = 529

// StatusError is a non-success HTTP response from a provider, other than
// a rejected credential (see [AuthError]). Branch on it with errors.As
// instead of matching the message.
type StatusError struct {
	// Provider names the provider that answered.
	Provider string
	// StatusCode is the HTTP status it returned.
	StatusCode int
	// Code is the provider's machine-readable error code or type (such as
	// "rate_limit_error"); empty when the body carries none.
	Code string
	// Detail is the provider's message, bounded and on one line.
	Detail string
	// RetryAfter is the wait the provider asked for; zero when it gave none.
	RetryAfter time.Duration
}

// Error keeps the "status NNN" wording that callers written before this
// type match on.
func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: api error: status %d: %s", e.Provider, e.StatusCode, e.Detail)
}

// codeQuotaExhausted is OpenAI's code for a 429 that no wait will cure:
// the account has run out of credit.
const codeQuotaExhausted = "insufficient_quota"

// Temporary reports whether the same request may succeed later: a
// timeout, a rate limit or a failure on the provider's side.
func (e *StatusError) Temporary() bool {
	if e.Code == codeQuotaExhausted {
		return false
	}
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, statusOverloaded:
		return true
	}
	return false
}

// retryAfter reads a Retry-After header: delay seconds or an HTTP date.
// Zero for a missing, malformed or already past value.
func retryAfter(header string, now time.Time) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return max(0, time.Duration(secs)*time.Second)
	}
	if at, err := http.ParseTime(header); err == nil {
		return max(0, at.Sub(now))
	}
	return 0
}

// codeOf extracts the provider's error code, or failing that its error
// type, from an error body. Empty when the body is not an error envelope.
func codeOf(body []byte) string {
	var envelope struct {
		Error apiError `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	return oneLine(cmp.Or(string(envelope.Error.Code), envelope.Error.Type))
}
