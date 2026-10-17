// Package upstream is how a server reads answers from the endpoints it sends
// credentials to: a product authorization server's token endpoint (the
// RFC 8693 exchange, and a stdio login's code, refresh and registration
// calls) and any identity probe that presents a freshly minted token. It
// holds the one error type whose text is safe to log, report to Sentry or
// show a client, and the bounds on the durations those endpoints state.
//
// Everything such an endpoint returns is endpoint-controlled text, and a
// broken or misrouted endpoint (or a proxy in front of it) can echo what the
// request carried: the subject token, the client secret or assertion, a
// refresh token. The Go standard library quotes that text into its own errors
// in more places than a call site can see — a malformed redirect Location, a
// malformed status line or chunked trailer, a JSON number too large for its
// field — so fixing call sites one by one keeps missing the next one. A
// Failure therefore never renders its cause: its text is built only from
// fixed phrases, the status, an allowlisted OAuth error code and a scrubbed,
// bounded description.
//
// Extracted from basecamp-mcp-server's internal/upstream, where it is the
// boundary for bc3's token endpoint; comments naming bc3 describe that
// contract.
package upstream

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxDescriptionBytes bounds the endpoint-supplied description a Failure
// renders. The description is a diagnostic hint for an operator, not a
// payload.
const MaxDescriptionBytes = 200

// minTokenRun is the shortest run of token-alphabet characters a Failure's
// description redacts. bc3's tokens, JWT client assertions and generated
// client secrets are all longer; English words in an OAuth error_description
// are all shorter.
const minTokenRun = 20

// Failure is a failed call to a credentialed endpoint. Its Error text is safe
// for any sink. The cause stays reachable through errors.Is and errors.As
// (context.DeadlineExceeded, *url.Error, ...), but Failure deliberately has no
// Unwrap method: Sentry's Go SDK walks Unwrap and Cause chains and records each
// link's Error text as its own exception, so an unwrappable cause would put
// exactly the text this type withholds into every captured event.
type Failure struct {
	// Op names the call in fixed text, such as "token exchange
	// infrastructure failure".
	Op string
	// Reason names the failure class in fixed text, such as "token
	// endpoint request failed". Never interpolate endpoint text into it.
	Reason string
	// Status is the HTTP status received, 0 when no response arrived.
	Status int
	// Code is the endpoint's OAuth error code. Only codes on the
	// enumerated allowlist (see SanitizeOAuthCode) are rendered.
	Code string
	// Description is the endpoint's error_description, already redacted of
	// the credentials the request submitted (see exchange.ParseOAuthError).
	// It is rendered scrubbed of control characters and token-shaped runs,
	// and bounded to MaxDescriptionBytes.
	Description string

	cause error
}

// New returns a Failure for a call that failed with cause, which may be nil.
func New(op, reason string, status int, cause error) *Failure {
	return &Failure{Op: op, Reason: reason, Status: status, cause: cause}
}

// Wrap makes f carry cause, for constructors that build a Failure as a
// value (an embedding type such as exchange.InfraError).
func (f *Failure) Wrap(cause error) { f.cause = cause }

func (f *Failure) Error() string {
	var b strings.Builder
	b.WriteString(f.Op)
	if f.Status != 0 {
		fmt.Fprintf(&b, " (status %d)", f.Status)
	}
	if f.Code != "" {
		fmt.Fprintf(&b, " [%s]", SanitizeOAuthCode(f.Code))
	}
	if f.Reason != "" {
		b.WriteString(": ")
		b.WriteString(f.Reason)
	}
	if d := ScrubDescription(f.Description); d != "" {
		fmt.Fprintf(&b, " (%s)", d)
	}
	return b.String()
}

// Is reports whether the cause matches target, so errors.Is reaches
// context.DeadlineExceeded and friends without an Unwrap method.
func (f *Failure) Is(target error) bool {
	return f.cause != nil && errors.Is(f.cause, target)
}

// As finds the first error in the cause's chain that matches target. A caller
// that asks for the raw cause by type gets it, and takes on its text.
func (f *Failure) As(target any) bool {
	return f.cause != nil && errors.As(f.cause, target)
}

// ScrubDescription renders endpoint-supplied text for a log line: control
// characters stripped, every run of at least minTokenRun token-alphabet
// characters replaced with [redacted], and the result bounded to
// MaxDescriptionBytes. Redaction runs over the whole text before the bound,
// so the bound can never cut a token down to a prefix that escapes the run
// length.
func ScrubDescription(s string) string {
	var stripped strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f && r != utf8.RuneError {
			stripped.WriteRune(r)
		}
	}
	text := stripped.String()

	var b strings.Builder
	run := 0
	start := 0
	flush := func(end int) {
		if run >= minTokenRun {
			b.WriteString("[redacted]")
		} else {
			b.WriteString(text[start:end])
		}
	}
	for i := 0; i < len(text); i++ {
		if isTokenByte(text[i]) {
			if run == 0 {
				start = i
			}
			run++
			continue
		}
		if run > 0 {
			flush(i)
			run = 0
		}
		b.WriteByte(text[i])
	}
	if run > 0 {
		flush(len(text))
	}

	out := strings.TrimSpace(b.String())
	if len(out) <= MaxDescriptionBytes {
		return out
	}
	cut := MaxDescriptionBytes
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + "…"
}

// isTokenByte reports whether c can appear in a bearer token, a JWT, a
// base64 or base64url secret, or a form-encoded one.
func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~+/=%", c) >= 0
}

// knownOAuthCodes is the closed set of error codes this server passes
// through: RFC 6749 §5.2, RFC 7591 §3.2.2 (dynamic client registration, which
// the stdio login path routes through this same boundary), RFC 8693, RFC 9449,
// and the bc3 delegation contract (docs/bc3-asks.md). Everything else
// collapses to "unrecognized_error" before it can reach any error string, log,
// or transcript.
//
// An ENUMERATED list, not a shape check — the shape check was tried and had a
// hole exactly the size of a credential: bc_at_* tokens are lowercase, digits,
// and underscores, so "looks like an OAuth code" admits precisely the byte
// string this guard exists to keep out of logs. The cost of the list is that a
// future bc3 code lands in the unrecognized bucket until the contract doc (and
// this set) updates — which is the honest telemetry for an unknown code
// anyway.
var knownOAuthCodes = map[string]bool{
	"invalid_request":         true,
	"invalid_client":          true,
	"invalid_grant":           true,
	"unauthorized_client":     true,
	"unsupported_grant_type":  true,
	"invalid_scope":           true,
	"invalid_target":          true,
	"invalid_dpop_proof":      true,
	"use_dpop_nonce":          true,
	"server_error":            true,
	"temporarily_unavailable": true,
	"access_denied":           true,
	// RFC 7591 §3.2.2 registration errors — a fixed vocabulary, like the rest.
	"invalid_redirect_uri":          true,
	"invalid_client_metadata":       true,
	"invalid_software_statement":    true,
	"unapproved_software_statement": true,
}

// UnrecognizedOAuthCode is what any code off the allowlist collapses to.
const UnrecognizedOAuthCode = "unrecognized_error"

// SanitizeOAuthCode admits only enumerated codes; see knownOAuthCodes.
func SanitizeOAuthCode(code string) string {
	if knownOAuthCodes[code] {
		return code
	}
	return UnrecognizedOAuthCode
}
