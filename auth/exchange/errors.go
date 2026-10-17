package exchange

import (
	"errors"
	"fmt"
	"time"
)

// TerminalError reports that the AS rejected the SUBJECT TOKEN itself
// (invalid, expired, revoked, or not audienced to this server). The HTTP
// middleware answers it with 401 invalid_token: the MCP client must
// re-authorize. OAuth error code "invalid_grant" is the terminal signal.
type TerminalError struct {
	Code        string
	Description string
}

func (e *TerminalError) Error() string {
	// Description is ENDPOINT-CONTROLLED text and deliberately excluded: this
	// string travels into logs and probe transcripts, and a broken endpoint
	// echoing its input (subject token included) must not ride along. The
	// Description field stays available for callers that sanitize it
	// explicitly (the middleware's capped, token-redacted telemetry field).
	return fmt.Sprintf("token exchange: subject token rejected (%s)", e.Code)
}

// InfraError reports every non-terminal exchange failure: the token
// endpoint is unreachable, timing out, 5xx, or malformed, OR the exchange
// is misconfigured on OUR side (invalid_client, invalid_request,
// unauthorized_client, unsupported_grant_type, invalid_target, ...). The
// HTTP middleware answers it with 503 + Retry-After: an outage or our own
// misconfiguration must never tell every MCP client to re-consent.
type InfraError struct {
	// Status is the HTTP status received, 0 when the request never
	// completed (transport error, timeout).
	Status int
	// Code is the OAuth error code when the AS supplied one.
	Code string
	// RetryAfter is the endpoint's requested backoff, parsed from a
	// delta-seconds Retry-After header on 429 responses. Zero when
	// unknown.
	RetryAfter time.Duration
	// Err is the underlying cause; Unwrap exposes it so errors.Is reaches
	// context.DeadlineExceeded and friends.
	Err error
}

func (e *InfraError) Error() string {
	msg := "token exchange infrastructure failure"
	if e.Status != 0 {
		msg = fmt.Sprintf("%s (status %d)", msg, e.Status)
	}
	if e.Code != "" {
		msg = fmt.Sprintf("%s [%s]", msg, e.Code)
	}
	if e.Err != nil {
		msg = fmt.Sprintf("%s: %v", msg, e.Err)
	}
	return msg
}

func (e *InfraError) Unwrap() error { return e.Err }

// IsTerminal reports whether err (anywhere in its chain) is a
// TerminalError - the subject token was rejected.
func IsTerminal(err error) bool {
	var t *TerminalError
	return errors.As(err, &t)
}

// IsInfra reports whether err (anywhere in its chain) is an InfraError -
// retry later, never re-authorize.
func IsInfra(err error) bool {
	var i *InfraError
	return errors.As(err, &i)
}
