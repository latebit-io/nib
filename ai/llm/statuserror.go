package llm

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	return temporaryStatus(e.StatusCode)
}

// temporaryStatus reports whether an HTTP status is a timeout, a rate
// limit or a failure on the provider's side.
func temporaryStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, statusOverloaded:
		return true
	}
	return false
}

// StreamError is a failure the provider reported inside a successful
// (HTTP 200) response stream, such as Anthropic's overloaded_error. It is
// the terminal [StreamEvent.Err]; what was streamed before it is partial.
type StreamError struct {
	// Provider names the provider that reported it.
	Provider string
	// Code is the provider's machine-readable error code or type (some
	// relays send an HTTP status as text); empty when it gave none.
	Code string
	// Detail is the provider's code and message, bounded and on one line.
	Detail string

	what  string // how the provider framed it; empty means "stream error"
	class string // the provider's error type, which Code hides when both came
}

// Error keeps the wording these failures had before they were typed.
func (e *StreamError) Error() string {
	text := cmp.Or(e.what, "stream error") + ": " + e.Detail
	if e.Provider == "" {
		return text
	}
	return e.Provider + ": " + text
}

// temporaryCodes are the error codes and types providers stream for a
// failure on their side or a rate limit.
var temporaryCodes = []string{
	"overloaded_error", "rate_limit_error", "api_error", "timeout_error", // Anthropic
	"server_error", "rate_limit_exceeded", // OpenAI
}

// Temporary reports whether the same request may succeed later, going by
// the provider's code and its error type. A numeric Code is read as the
// HTTP status a relay passed on.
func (e *StreamError) Temporary() bool {
	if e.Code == codeQuotaExhausted || e.class == codeQuotaExhausted {
		return false
	}
	if status, err := strconv.Atoi(e.Code); err == nil {
		return temporaryStatus(status)
	}
	return slices.Contains(temporaryCodes, e.Code) || slices.Contains(temporaryCodes, e.class)
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
	return envelope.Error.code()
}
