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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"unicode/utf8"
)

// MaxDescriptionBytes bounds the endpoint-supplied description a Failure
// renders. The description is a diagnostic hint for an operator, not a
// payload.
const MaxDescriptionBytes = 200

// minTokenRun is the shortest run of token-alphabet characters a Failure's
// description redacts. bc3's tokens, JWT client assertions and generated
// client secrets are all longer; English words in an OAuth error_description
// are all shorter. A shorter run is kept, so a fragment of a credential under
// this length can render if an endpoint truncates its own echo: the guarantee
// is that no usable credential renders, not that no byte of one does. Every
// credential the request submitted is also removed whole, at any length, by
// exchange.ParseOAuthError before the description gets here.
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
	if c := causeClass(f.cause); c != "" {
		b.WriteString(": ")
		b.WriteString(c)
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

// causeClass names what kind of failure cause is, in fixed text, so an
// operator reading a log line can still tell a timeout from a refused
// connection from a bad certificate without the cause's own text. Every
// phrase is chosen here; nothing is read out of the cause but its type and
// the identity of sentinel errors. The one exception is a JSON type error's
// Field, which is the path of the Go struct field being decoded: this
// server's own names, never response bytes.
func causeClass(err error) string {
	if err == nil {
		return ""
	}
	var (
		netErr     net.Error
		dnsErr     *net.DNSError
		opErr      *net.OpError
		unknownCA  x509.UnknownAuthorityError
		certErr    x509.CertificateInvalidError
		hostErr    x509.HostnameError
		recordErr  tls.RecordHeaderError
		syntaxErr  *json.SyntaxError
		typeErr    *json.UnmarshalTypeError
		maxBytes   *http.MaxBytesError
		protoError *http.ProtocolError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.As(err, &unknownCA), errors.As(err, &certErr), errors.As(err, &hostErr):
		return "TLS certificate verification failed"
	case errors.As(err, &recordErr):
		return "TLS handshake failed"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "response ended early"
	case errors.As(err, &syntaxErr):
		return "response is not valid JSON"
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return "unexpected JSON value for " + typeErr.Field
		}
		return "unexpected JSON value"
	case errors.As(err, &maxBytes):
		return "response too large"
	case errors.As(err, &protoError):
		return "malformed HTTP response"
	case errors.As(err, &opErr):
		return "network error during " + opErr.Op
	default:
		return ""
	}
}

// ScrubDescription renders endpoint-supplied text for a log line: control
// and everything but printable ASCII stripped (PrintableASCII), every run of at least minTokenRun token-alphabet
// characters replaced with [redacted], and the result bounded to
// MaxDescriptionBytes. Redaction runs over the whole text before the bound,
// so the bound can never cut a token down to a prefix that escapes the run
// length.
func ScrubDescription(s string) string {
	text := PrintableASCII(s)

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

// PrintableASCII keeps only printable ASCII (space through tilde) from s.
// Redaction runs on the result, so nothing invisible can sit inside an
// echoed credential and split it into pieces that exact matching and the
// token-run length both miss, for a viewer or a normalizer that drops
// invisible characters to put back together. An allowlist, because the
// invisible set is not: C0 and DEL, then C1 and format characters, then
// variation selectors were each found in turn on review, and Unicode has
// more. OAuth error descriptions are ASCII by specification (RFC 6749
// §5.2), so nothing a conformant endpoint says is lost.
func PrintableASCII(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
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
