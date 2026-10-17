package exchange

import (
	"errors"
	"fmt"
	"time"

	"github.com/basecamp/mcp/auth/upstream"
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
//
// Its text comes from the embedded upstream.Failure, which renders the
// status, the allowlisted Code, a fixed reason and a scrubbed description,
// and never the cause: see package upstream for why that is the boundary.
// errors.Is and errors.As still reach the cause (context.DeadlineExceeded,
// *url.Error, ...).
type InfraError struct {
	upstream.Failure
	// RetryAfter is the endpoint's requested backoff, parsed from a
	// delta-seconds Retry-After header on 429 responses and bounded to
	// upstream.MaxRetryAfter. Zero when unknown.
	RetryAfter time.Duration
}

// infraOp names every InfraError's call.
const infraOp = "token exchange infrastructure failure"

// newInfra builds an InfraError. reason must be fixed text.
func newInfra(status int, code, reason string, cause error) *InfraError {
	e := &InfraError{Failure: upstream.Failure{Op: infraOp, Reason: reason, Status: status, Code: code}}
	e.Wrap(cause)
	return e
}

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
