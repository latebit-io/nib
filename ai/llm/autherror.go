package llm

import (
	"encoding/json"
	"fmt"
)

// AuthError indicates a provider rejected the request's credentials
// (HTTP 401). It is distinct from transient transport or server errors so
// callers can branch on credential failure with errors.As rather than
// string-matching an opaque message.
//
// Code carries the provider-specific machine code when the response body
// supplies one (e.g. "token_revoked" from the ChatGPT/Codex endpoint);
// it is empty when the body has no structured code. Message is the
// human-readable detail from the body, when present.
type AuthError struct {
	// Provider names the provider that rejected the credentials (e.g.
	// "codex"), so a surfaced error identifies which login is stale.
	Provider string
	// StatusCode is the HTTP status the provider returned (always 401 for
	// the cases this type is constructed for).
	StatusCode int
	// Code is the provider's machine-readable error code when present
	// (e.g. "token_revoked"). May be empty.
	Code string
	// Message is the provider's human-readable detail when present. May
	// be empty.
	Message string
}

// Error renders an actionable message rather than dumping the raw provider
// body. The frontend surfaces this string directly, so it names the
// provider, the cause, and the recovery path.
func (e *AuthError) Error() string {
	detail := e.Message
	if e.Code != "" {
		if detail != "" {
			detail = e.Code + ": " + detail
		} else {
			detail = e.Code
		}
	}
	if detail == "" {
		detail = fmt.Sprintf("status %d", e.StatusCode)
	}
	return fmt.Sprintf("%s authentication failed (%s) — re-authenticate, then restart", e.Provider, detail)
}

// CredentialInvalidator is an optional capability of an [Auth]. A provider
// calls Invalidate after the server rejects the credential as unusable
// (an HTTP 401 — a revoked or otherwise dead OAuth token) so the backing
// store discards it. Discarding lets the next launch detect "no
// credential" and re-offer the connect flow, instead of replaying a token
// the server has already rejected.
//
// The capability is optional: static API-key auth does not implement it,
// so a 401 there invalidates nothing (there is no stored token to clear).
type CredentialInvalidator interface {
	// Invalidate discards the current credential. Implementations log
	// their own failures; the caller treats the call as best-effort.
	Invalidate()
}

// parseAuthError builds an [AuthError] from a provider's 401 response
// body. It tolerates a missing or malformed body — Code and Message stay
// empty in that case, leaving the status code as the only detail. The
// shape parsed matches the OpenAI/Codex error envelope:
//
//	{"error": {"message": "...", "code": "token_revoked"}, "status": 401}
func parseAuthError(provider string, statusCode int, body []byte) *AuthError {
	e := &AuthError{Provider: provider, StatusCode: statusCode}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
	}
	return e
}
