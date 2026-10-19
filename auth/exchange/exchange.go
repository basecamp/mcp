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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/basecamp/mcp/auth/upstream"
)

const (
	grantTypeTokenExchange      = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- RFC 8693 URN, not a credential
	subjectTokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"   // #nosec G101 -- RFC 8693 URN, not a credential

	// MaxResponseBytes caps how much of a token endpoint response is read.
	// The AS is trusted but a misrouted or broken endpoint must not induce
	// an unbounded read; a longer body is an infrastructure failure, never
	// parsed. Exported so
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
	// Scope is the scope of the issued token as the AS reported it, the
	// space-delimited list RFC 6749 §3.3 defines, untouched. Empty when the
	// response omitted it. RFC 8693 §2.2.1 lets an AS omit it when it is
	// identical to the scope requested, and this exchanger requests none,
	// so an empty Scope is "unknown", never "nothing": a caller that filters
	// what it serves by scope must treat it as the narrowest grant.
	Scope string
}

// New validates cfg and returns an Exchanger. Every required field fails
// closed: a half-configured exchanger must never be constructed and then
// misbehave per-request.
func New(cfg Config) (*Exchanger, error) {
	if cfg.TokenURL == "" {
		return nil, errors.New("exchange: Config.TokenURL is required")
	}
	if err := ValidateEndpointURL(cfg.TokenURL); err != nil {
		return nil, fmt.Errorf("exchange: Config.TokenURL: %w", err)
	}
	if cfg.Auth == nil {
		return nil, errors.New("exchange: Config.Auth is required")
	}
	// A private_key_jwt assertion's audience is the URL it was built for;
	// built for another, every assertion this exchanger posts is refused.
	if p, ok := cfg.Auth.(*privateKeyJWT); ok && p != nil && p.tokenURL != cfg.TokenURL {
		return nil, errors.New("exchange: Config.Auth is a private_key_jwt authenticator built for a different token URL " +
			"than Config.TokenURL (values withheld): its assertions' audience would never match")
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
	// The client's Timeout bounds each request; a nonce retry is a second
	// one, so the exchange as a whole gets the same bound, shared by both
	// attempts, rather than twice it. A caller deadline that is earlier
	// still wins: WithTimeout keeps the parent's when it comes first.
	if e.httpClient.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.httpClient.Timeout)
		defer cancel()
	}
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
			return nil, newInfra(nr.status, "use_dpop_nonce", "token endpoint demanded a fresh DPoP nonce after a nonce-bearing retry", nil)
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
		return nil, newInfra(0, "", "applying client authentication failed", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, newInfra(0, "", "building the token request failed", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if e.signer != nil {
		// No ath: the proof accompanies client authentication here, not an
		// access-token presentation (RFC 9449 §4.3).
		proof, err := e.mintProof(nonce)
		if err != nil {
			return nil, newInfra(0, "", "minting the DPoP proof failed", err)
		}
		req.Header.Set("DPoP", proof)
	}

	// Every failure below keeps its cause for errors.Is (a timeout, a
	// refused connection) but renders only fixed text: the transport's own
	// errors quote endpoint-controlled bytes (a redirect Location, a
	// malformed status line or chunked trailer), any of which can echo the
	// subject token or client secret. See package upstream.
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if resp != nil {
			// A refused redirect (CheckRedirect above).
			_ = resp.Body.Close()
			return nil, newInfra(resp.StatusCode, "", "token endpoint responded with a redirect; refusing to follow", err)
		}
		return nil, newInfra(0, "", "token endpoint request failed", err)
	}
	defer resp.Body.Close()

	// One byte past the cap tells a body that ends at it from one that runs
	// on: a complete JSON prefix followed by padding and more bytes must not
	// parse as an answer (an invalid_grant that forces re-authorization, or
	// a token).
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, newInfra(resp.StatusCode, "", "reading the token endpoint response failed", err)
	}
	if len(body) > MaxResponseBytes {
		return nil, newInfra(resp.StatusCode, "", "token endpoint response exceeded the size limit", nil)
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
		ie := newInfra(resp.StatusCode, "", "token endpoint rate limited the exchange", nil)
		ie.RetryAfter = upstream.ParseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, ie
	case http.StatusBadRequest, http.StatusUnauthorized:
		return nil, e.classifyOAuthError(resp, body, form)
	default:
		return nil, newInfra(resp.StatusCode, "", "token endpoint returned an unexpected status", nil)
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

// sanitizeOAuthCode admits only enumerated OAuth error codes; the list lives
// with the error type that renders them (upstream.SanitizeOAuthCode).
var sanitizeOAuthCode = upstream.SanitizeOAuthCode

// nonceRequiredError is internal control flow for the nonce dance; it
// never escapes Exchange.
type nonceRequiredError struct {
	nonce  string
	status int
}

func (e *nonceRequiredError) Error() string {
	return "token endpoint requires a DPoP nonce"
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
		Scope             string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, newInfra(http.StatusOK, "", "token response is not the JSON this server expects", err)
	}
	if tr.AccessToken == "" {
		return nil, newInfra(http.StatusOK, "", "token response missing access_token", nil)
	}
	if !headerSafeToken(tr.AccessToken) {
		// A token that can't go in an Authorization header would fail every
		// API request it is cached for; refuse it here, where it is still an
		// exchange failure.
		return nil, newInfra(http.StatusOK, "", "token response access_token is not a printable token", nil)
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
		return nil, newInfra(http.StatusOK, "", "token response issued_token_type is not the access-token URN", nil)
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") && !strings.EqualFold(tr.TokenType, "DPoP") {
		return nil, newInfra(http.StatusOK, "", "token response token_type is not a presentation scheme this server implements", nil)
	}

	tok := &Token{
		AccessToken:       tr.AccessToken,
		TokenType:         tr.TokenType,
		SubjectTokenBound: tr.SubjectTokenBound,
		Scope:             tr.Scope,
	}
	// Absent expires_in means "lifetime unknown" (ExpiresAt zero, cached
	// under the TTL cap). An EXPLICIT zero or negative value is different:
	// the AS just said the token has no remaining life, and treating that
	// as unknown would cache a dead credential for the full cap. Malformed
	// success, infra class. A lifetime past upstream.MaxLifetime is taken
	// at the bound: multiplied out unbounded it overflows into a token that
	// is already expired, and a cache TTL caps it far below the bound
	// anyway.
	if tr.ExpiresIn != nil {
		if *tr.ExpiresIn <= 0 {
			return nil, newInfra(http.StatusOK, "", "token response expires_in is not positive", nil)
		}
		tok.ExpiresAt = time.Now().Add(upstream.Lifetime(*tr.ExpiresIn))
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
		return newInfra(status, "", "token endpoint returned an unparseable OAuth error response", nil)
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
			return newInfra(status, code, "use_dpop_nonce without a DPoP-Nonce response header", nil)
		}
		return &nonceRequiredError{nonce: nonce, status: status}
	default:
		ie := newInfra(status, code, "token endpoint rejected the exchange request", nil)
		ie.Description = description
		return ie
	}
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
