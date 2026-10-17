// Package exchange implements an RFC 8693 OAuth 2.0 token-exchange client
// for a hosted MCP server whose only route to its product API is a
// delegation exchange at the product's authorization server. Each inbound
// MCP access token is presented as subject_token, together with the
// server's client credentials and an optional RFC 8707 resource indicator,
// and the AS returns a short-lived access token for the product API.
//
// Exchange success IS inbound validation: the AS rejects a subject token
// that is invalid, expired, revoked, or not audienced to this server, so no
// separate introspection step exists. The error taxonomy keeps the two
// failure families apart - a rejected subject token (TerminalError) sends
// the MCP client back through authorization, while an endpoint outage or a
// misconfiguration of OUR client (InfraError) must never do that.
//
// The contract is bc3's delegation exchange (basecamp-mcp-server's
// docs/bc3-asks.md, where this package was proven); comments naming bc3
// describe that contract, which other product authorization servers copy
// verbatim (Fizzy's hosted plan, phase C).
package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	grantTypeTokenExchange      = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- RFC 8693 URN, not a credential
	subjectTokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"   // #nosec G101 -- RFC 8693 URN, not a credential

	// MaxResponseBytes caps how much of a token endpoint response is read.
	// The AS is trusted but a misrouted or broken endpoint must not induce
	// an unbounded read; a truncated body simply fails to parse. Exported so
	// a product's stdio login path bounds its own reads identically.
	MaxResponseBytes = 64 << 10

	// DefaultTimeout bounds the whole exchange round trip when the caller
	// supplies no HTTP client (exported so a caller supplying one can
	// keep it). Matches the validator's posture: an AS that
	// takes longer than this is an outage, not a wait.
	DefaultTimeout = 10 * time.Second
)

// ProofSigner mints DPoP proofs for the token endpoint. Satisfied by
// *dpop.Key; kept as a local interface so this package needs no dpop
// import. ProofWithNonce serves the RFC 9449 §8 nonce dance: same proof,
// plus the AS-supplied nonce claim.
type ProofSigner interface {
	Proof(method, htu, accessToken string) (string, error)
	ProofWithNonce(method, htu, accessToken, nonce string) (string, error)
	Thumbprint() string
}

// Config carries everything an Exchanger needs. TokenURL and Auth are
// required; New fails closed on either missing.
type Config struct {
	// TokenURL is the AS token endpoint the exchange is POSTed to.
	TokenURL string
	// Resource is the OPTIONAL RFC 8707 resource indicator: where the
	// minted token will be USED (API origin or account URN), not this
	// server's identity - bc3 derives our audience binding from client
	// registration. When empty the resource form param is omitted
	// entirely.
	Resource string
	// Auth applies this server's client authentication to each request.
	Auth ClientAuthenticator
	// HTTPClient overrides the default 10s-timeout client when set.
	HTTPClient *http.Client
	// Signer, when set, DPoP-binds exchanged tokens to its key.
	Signer ProofSigner
}

// Exchanger performs RFC 8693 token exchanges against a single token
// endpoint. Safe for concurrent use.
type Exchanger struct {
	tokenURL   string
	resource   string
	auth       ClientAuthenticator
	httpClient *http.Client
	signer     ProofSigner
}

// Token is the result of a successful exchange.
type Token struct {
	AccessToken string
	// TokenType is as returned by the AS: bc3 returns "DPoP" for
	// proof-bound tokens, "Bearer" otherwise. token_type is
	// case-insensitive (RFC 6749 §5.1) - callers comparing it must use a
	// case-insensitive comparison.
	TokenType string
	// ExpiresAt is zero when the response carried no expires_in.
	ExpiresAt time.Time
	// SubjectTokenBound is bc3's extension member reporting whether the
	// SUBJECT token was DPoP-bound. Nil when the response omitted it
	// (pre-contract endpoints). This is the accept-mode telemetry channel:
	// it reports binding without ever risking a terminal error the way an
	// eagerly-sent subject_token_dpop_jkt would, and the bound fraction of
	// inbound traffic is what times the accept -> require flip.
	SubjectTokenBound *bool
}

// New validates cfg and returns an Exchanger. Every required field fails
// closed: a half-configured exchanger must never be constructed and then
// misbehave per-request.
func New(cfg Config) (*Exchanger, error) {
	if cfg.TokenURL == "" {
		return nil, errors.New("exchange: Config.TokenURL is required")
	}
	// The exchange POSTs the subject token and this client's credentials, so
	// the endpoint must be TLS (RFC 6749 §3.2); plain http is accepted only
	// on a loopback host, for local development and tests.
	if u, err := url.Parse(cfg.TokenURL); err != nil || u.Host == "" ||
		!(u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, fmt.Errorf("exchange: Config.TokenURL must be an https URL (http only on loopback), got %q", cfg.TokenURL)
	}
	if cfg.Auth == nil {
		return nil, errors.New("exchange: Config.Auth is required")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	// Token endpoints do not redirect, and following one would REPLAY this
	// POST - subject token and client credentials included - to whatever
	// origin Location names (307/308 preserve the body). A shallow clone
	// keeps the caller's transport and timeout while refusing redirects, so
	// any 3xx surfaces as an InfraError instead of a cross-origin replay.
	httpClient = &http.Client{
		Transport: httpClient.Transport,
		Jar:       httpClient.Jar,
		Timeout:   httpClient.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("token endpoint responded with a redirect; refusing to follow")
		},
	}

	return &Exchanger{
		tokenURL:   cfg.TokenURL,
		resource:   cfg.Resource,
		auth:       cfg.Auth,
		httpClient: httpClient,
		signer:     cfg.Signer,
	}, nil
}

// Exchange presents subjectToken to the token endpoint and returns the
// short-lived token bc3 mints for it. Failures are always a *TerminalError
// (the subject token itself was rejected - re-authorization required) or a
// *InfraError (endpoint unreachable/broken/rate-limiting, or our client
// misconfigured - retryable, never a re-consent signal). Error strings
// never carry token values.
func (e *Exchanger) Exchange(ctx context.Context, subjectToken string) (*Token, error) {
	return e.ExchangeWithBinding(ctx, subjectToken, "")
}

// ExchangeWithBinding additionally sends subject_token_dpop_jkt, asking bc3
// to confirm that the key the MCP client proved possession of is the key its
// token is bound to (docs/bc3-asks.md ask 6). STRICT semantics by contract:
// sending a jkt for an UNBOUND subject token is invalid_grant, so callers
// send one only when the deployment requires bound tokens (inbound DPoP
// require mode) - never speculatively.
func (e *Exchanger) ExchangeWithBinding(ctx context.Context, subjectToken, subjectJKT string) (*Token, error) {
	tok, err := e.exchangeOnce(ctx, subjectToken, subjectJKT, "")

	// RFC 9449 §8 nonce dance: ONE deterministic retry with the
	// AS-supplied nonce, then stop. bc3's client rows don't require nonces
	// initially, but the dance must exist so they can flip the requirement
	// without breaking us (docs/bc3-asks.md ask #4).
	var nr *nonceRequiredError
	if errors.As(err, &nr) {
		tok, err = e.exchangeOnce(ctx, subjectToken, subjectJKT, nr.nonce)
		if errors.As(err, &nr) {
			// A second demand would loop forever; a nonce the AS just
			// issued being rejected is an endpoint malfunction.
			return nil, &InfraError{Status: nr.status, Code: "use_dpop_nonce", Err: errors.New("token endpoint demanded a fresh DPoP nonce after a nonce-bearing retry")}
		}
	}
	return tok, err
}

// exchangeOnce performs a single exchange round trip. The form is built
// and client authentication applied fresh per attempt: a nonce retry must
// carry a new private_key_jwt assertion (fresh jti), never a replay.
func (e *Exchanger) exchangeOnce(ctx context.Context, subjectToken, subjectJKT, nonce string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", grantTypeTokenExchange)
	form.Set("subject_token", subjectToken)
	form.Set("subject_token_type", subjectTokenTypeAccessToken)
	if subjectJKT != "" {
		form.Set("subject_token_dpop_jkt", subjectJKT)
	}
	// resource says where the minted token will be USED (RFC 8707); it is
	// not our identity, so an unset value is omitted rather than defaulted.
	if e.resource != "" {
		form.Set("resource", e.resource)
	}
	if err := e.auth.Authenticate(form); err != nil {
		return nil, &InfraError{Err: fmt.Errorf("applying client authentication: %w", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &InfraError{Err: fmt.Errorf("building token request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if e.signer != nil {
		// No ath: the proof accompanies client authentication here, not an
		// access-token presentation (RFC 9449 §4.3).
		proof, err := e.mintProof(nonce)
		if err != nil {
			return nil, &InfraError{Err: fmt.Errorf("minting DPoP proof: %w", err)}
		}
		req.Header.Set("DPoP", proof)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if resp != nil {
			// A refused redirect: the *url.Error embeds the Location target
			// (endpoint-controlled — it could echo the subject token), so a
			// fixed message replaces the wrap here.
			_ = resp.Body.Close()
			return nil, &InfraError{Status: resp.StatusCode,
				Err: errors.New("token endpoint responded with a redirect; refusing to follow")}
		}
		// The cause stays reachable (context.DeadlineExceeded, connection
		// refused, ...) via errors.Is and errors.As, but its text does not
		// reach Error(): a redirect whose Location fails to parse returns
		// here with no response and the endpoint-controlled Location quoted
		// in the error, and that Location could echo the subject token.
		return nil, &InfraError{Err: redactTransportError(err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return nil, &InfraError{Status: resp.StatusCode, Err: fmt.Errorf("reading token endpoint response: %w", err)}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// A DPoP-Nonce header MAY accompany success responses; it is
		// deliberately ignored - each exchange is stateless, and one extra
		// round trip when a nonce becomes required is acceptable.
		return parseTokenResponse(body)
	case http.StatusTooManyRequests:
		// bc3 rate-limits the token endpoint (60/min per client_id+IP) and
		// serves escalating abuse blocks the same way. Always infra: a
		// throttled client must back off, never re-consent.
		return nil, &InfraError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        errors.New("token endpoint rate limited the exchange"),
		}
	case http.StatusBadRequest, http.StatusUnauthorized:
		return nil, e.classifyOAuthError(resp, body, form)
	default:
		return nil, &InfraError{Status: resp.StatusCode, Err: fmt.Errorf("token endpoint returned status %d", resp.StatusCode)}
	}
}

// mintProof selects the plain or nonce-bearing proof form.
func (e *Exchanger) mintProof(nonce string) (string, error) {
	// htu excludes query and fragment (RFC 9449 §4.2): a configured token
	// URL carrying either would otherwise put a target in the proof that
	// the AS's own comparison can never match. The HTTP request and the
	// client-assertion audience keep the configured URL as written.
	htu := e.tokenURL
	if u, err := url.Parse(e.tokenURL); err == nil {
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		htu = u.String()
	}
	if nonce != "" {
		return e.signer.ProofWithNonce(http.MethodPost, htu, "", nonce)
	}
	return e.signer.Proof(http.MethodPost, htu, "")
}

// knownOAuthCodes is the closed set of error codes this client passes
// through: RFC 6749 §5.2, RFC 7591 §3.2.2 (dynamic client registration,
// which the stdio login path routes through this same boundary), RFC 8693,
// RFC 9449, and the bc3 delegation contract (docs/bc3-asks.md). Everything
// else collapses to "unrecognized_error" before it can reach any error
// string, log, or transcript.
//
// An ENUMERATED list, not a shape check — the shape check was tried and had
// a hole exactly the size of a credential: bc_at_* tokens are lowercase,
// digits, and underscores, so "looks like an OAuth code" admits precisely
// the byte string this guard exists to keep out of logs. The cost of the
// list is that a future bc3 code lands in the unrecognized bucket until the
// contract doc (and this set) updates — which is the honest telemetry for
// an unknown code anyway, and updating the tracker on contract deltas is
// already this repo's workflow.
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

// sanitizeOAuthCode admits only enumerated codes; see knownOAuthCodes.
func sanitizeOAuthCode(code string) string {
	if knownOAuthCodes[code] {
		return code
	}
	return "unrecognized_error"
}

// nonceRequiredError is internal control flow for the nonce dance; it
// never escapes Exchange.
type nonceRequiredError struct {
	nonce  string
	status int
}

func (e *nonceRequiredError) Error() string {
	return "token endpoint requires a DPoP nonce"
}

// parseRetryAfter reads Retry-After in its delta-seconds form only; the
// HTTP-date form is ignored (bc3 sends delta-seconds). Zero means unknown.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || secs < 0 || secs > maxDurationSeconds {
		// Out of time.Duration's range would overflow to a negative
		// backoff; such a value is no usable instruction, so it is unknown.
		return 0
	}
	return time.Duration(secs) * time.Second
}

// parseTokenResponse decodes a 200 body. A 200 that fails to yield a
// non-empty access_token is a broken AS, not a rejected subject token, so
// every failure here is infrastructure.
func parseTokenResponse(body []byte) (*Token, error) {
	var tr struct {
		AccessToken       string `json:"access_token"`
		TokenType         string `json:"token_type"`
		IssuedTokenType   string `json:"issued_token_type"`
		ExpiresIn         *int64 `json:"expires_in"`
		SubjectTokenBound *bool  `json:"subject_token_bound"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		// Fixed text: a decode error quotes the offending value (a number
		// overflowing expires_in is printed whole), and a broken endpoint
		// could echo a credential there.
		return nil, &InfraError{Status: http.StatusOK, Err: errors.New("malformed token response")}
	}
	if tr.AccessToken == "" {
		return nil, &InfraError{Status: http.StatusOK, Err: errors.New("token response missing access_token")}
	}
	if !headerSafeToken(tr.AccessToken) {
		// A token that can't go in an Authorization header would fail every
		// API request it is cached for; refuse it here, where it is still an
		// exchange failure.
		return nil, &InfraError{Status: http.StatusOK, Err: errors.New("token response access_token is not a printable token")}
	}
	// RFC 8693 §2.2.1: issued_token_type is REQUIRED, and this server only
	// knows how to wield an access token — anything else (a refresh token, a
	// mis-profiled response) must not be forwarded as an API credential.
	// token_type likewise names the presentation scheme; only the two this
	// server implements are usable, and a missing one would leave scheme
	// selection guessing.
	// The two profile fields are endpoint-controlled text and are NOT
	// interpolated into errors (same doctrine as the error/description
	// members): a broken endpoint echoing a token into either field must
	// not reach logs or transcripts. The fixed messages lose nothing — the
	// only diagnostic question is which field was wrong.
	if tr.IssuedTokenType != subjectTokenTypeAccessToken {
		return nil, &InfraError{Status: http.StatusOK,
			Err: errors.New("token response issued_token_type is not the access-token URN")}
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") && !strings.EqualFold(tr.TokenType, "DPoP") {
		return nil, &InfraError{Status: http.StatusOK,
			Err: errors.New("token response token_type is not a presentation scheme this server implements")}
	}

	tok := &Token{
		AccessToken:       tr.AccessToken,
		TokenType:         tr.TokenType,
		SubjectTokenBound: tr.SubjectTokenBound,
	}
	// Absent expires_in means "lifetime unknown" (ExpiresAt zero, cached
	// under the TTL cap). An EXPLICIT zero or negative value is different:
	// the AS just said the token has no remaining life, and treating that
	// as unknown would cache a dead credential for the full cap. Malformed
	// success, infra class.
	if tr.ExpiresIn != nil {
		// Fixed text again: the value is endpoint-controlled. A lifetime
		// past time.Duration's range would overflow into the past.
		if *tr.ExpiresIn <= 0 || *tr.ExpiresIn > maxDurationSeconds {
			return nil, &InfraError{Status: http.StatusOK,
				Err: errors.New("token response expires_in is not a positive lifetime in range")}
		}
		tok.ExpiresAt = time.Now().Add(time.Duration(*tr.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// classifyOAuthError forks a 400/401 OAuth error body into the taxonomy.
// Only invalid_grant means the SUBJECT TOKEN was rejected; every other
// code (invalid_client, invalid_request, unauthorized_client,
// unsupported_grant_type, ...) indicates OUR client or request is broken,
// and an unparseable body indicates the endpoint is. Neither may
// masquerade as a re-authorization signal to every MCP client. In
// particular invalid_target is bc3's answer to resource-indicator problems
// - OUR misconfiguration, so clients get 503, never a re-consent 401.
func (e *Exchanger) classifyOAuthError(resp *http.Response, body []byte, form url.Values) error {
	status := resp.StatusCode
	// ParseOAuthError is the single boundary that turns an endpoint-controlled
	// error body into a sanitized code and a credential-redacted description;
	// an empty code means the body carried no `error` member (unparseable or
	// non-OAuth), which for a delegation exchange is an endpoint malfunction.
	code, description := ParseOAuthError(body, form)
	if code == "" {
		return &InfraError{Status: status, Err: fmt.Errorf("unparseable OAuth error response (status %d)", status)}
	}
	switch {
	case code == "invalid_grant":
		return &TerminalError{Code: code, Description: description}
	case code == "use_dpop_nonce" && e.signer != nil:
		// The nonce MUST arrive in the DPoP-Nonce header (RFC 9449 §8);
		// its absence leaves nothing to retry with. Without a signer the
		// demand is nonsense (we sent no proof) and falls through to the
		// generic infra case.
		nonce := resp.Header.Get("DPoP-Nonce")
		if nonce == "" {
			return &InfraError{Status: status, Code: code, Err: errors.New("use_dpop_nonce without a DPoP-Nonce response header")}
		}
		return &nonceRequiredError{nonce: nonce, status: status}
	default:
		return &InfraError{Status: status, Code: code, Err: fmt.Errorf("token endpoint rejected the exchange request: %s", code)}
	}
}

// maxDurationSeconds is the most seconds a time.Duration holds.
const maxDurationSeconds = int64(math.MaxInt64 / int64(time.Second))

// isLoopbackHost reports whether host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// headerSafeToken reports whether tok is visible ASCII with no spaces, the
// most an access token can be and still go in an Authorization header
// (RFC 6750 §2.1's b64token is a subset).
func headerSafeToken(tok string) bool {
	for i := 0; i < len(tok); i++ {
		if tok[i] < 0x21 || tok[i] > 0x7e {
			return false
		}
	}
	return true
}

// transportError carries a token endpoint transport failure with a message
// built only from what this side knows: the cause's class, never its text.
type transportError struct {
	msg string
	err error
}

func (e *transportError) Error() string { return e.msg }
func (e *transportError) Unwrap() error { return e.err }

// redactTransportError names a transport failure by its class. Timeouts,
// cancellation and network errors (dial, DNS) name only this server's own
// endpoint; anything else, including a malformed redirect Location, is
// reported without its text.
func redactTransportError(err error) error {
	msg := "token endpoint request failed"
	var netErr net.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg += ": deadline exceeded"
	case errors.Is(err, context.Canceled):
		msg += ": canceled"
	case errors.As(err, &dnsErr):
		msg += ": " + dnsErr.Error()
	case errors.As(err, &opErr):
		msg += ": " + opErr.Error()
	case errors.As(err, &netErr) && netErr.Timeout():
		msg += ": timeout"
	}
	return &transportError{msg: msg, err: err}
}
