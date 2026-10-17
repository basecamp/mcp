// Package upstreamtest serves the hostile answers a broken credentialed
// endpoint can give, each reflecting what the request carried, so tests can
// assert that none of it reaches an error string, a log line or a
// client-visible result. It is one catalog for every caller (the token
// exchange, an identity probe, a stdio login), so a new way for the standard
// library to quote a response grows every caller's test at once.
//
// basecamp-mcp-server's copy also captures Sentry events; the toolkit takes no
// Sentry dependency, and upstream.Failure's lack of an Unwrap method (pinned
// by its own test) is what keeps a Sentry chain walk off the cause.
package upstreamtest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Planted is a token-shaped value no request carries. A hostile endpoint
// returns it beside what it echoes, standing for a credential the request
// did not submit (another user's token, a minted one), which form-based
// redaction cannot know to remove.
const Planted = "bc_at_PlantedTokenNoRequestCarried_7f3a9c2e1b" // #nosec G101 -- test fixture

// PlantedNumber stands in for an echoed credential where only a number
// fits (a JSON number field). Too large for int64, so a decoder's overflow
// error quotes it.
const PlantedNumber = "918273645546372819283746556" // #nosec G101 -- test fixture

// credentialFields are the request members an endpoint might echo.
var credentialFields = []string{
	"subject_token", "client_secret", "client_assertion",
	"refresh_token", "code", "code_verifier",
}

// Echo is every credential r carried, plus Planted, joined so the result
// stays in the token alphabet and fits in a URL path or a header line.
func Echo(r *http.Request) string {
	_ = r.ParseForm()
	parts := []string{Planted}
	for _, f := range credentialFields {
		if v := r.PostForm.Get(f); v != "" {
			parts = append(parts, v)
		}
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		if _, tok, ok := strings.Cut(auth, " "); ok {
			parts = append(parts, tok)
		}
	}
	return strings.Join(parts, "_")
}

// EchoNumber is the first all-digit credential r carried, or PlantedNumber.
func EchoNumber(r *http.Request) string {
	_ = r.ParseForm()
	for _, f := range credentialFields {
		if v := r.PostForm.Get(f); v != "" && strings.Trim(v, "0123456789") == "" {
			return v
		}
	}
	return PlantedNumber
}

// Kind names one hostile answer.
type Kind string

const (
	// MalformedLocation redirects to a Location that fails to parse; the
	// client's error quotes it.
	MalformedLocation Kind = "malformed redirect Location"
	// Redirect redirects to a valid Location the client refuses to follow;
	// the refusal's *url.Error carries the target.
	Redirect Kind = "refused redirect"
	// MalformedStatus answers with a status line whose code is the echo.
	MalformedStatus Kind = "malformed status line"
	// MalformedHeader answers with a header line that has no colon.
	MalformedHeader Kind = "malformed header line"
	// MalformedTrailer answers 200 with a chunked body whose trailer has no
	// colon; reading the body fails quoting it.
	MalformedTrailer Kind = "malformed chunked trailer"
	// HugeNumber answers 200 with JSON whose number fields are too large
	// for int64; the decoder's error quotes the number.
	HugeNumber Kind = "number overflow"
	// NegativeLifetime answers 200 with a well-formed token whose
	// expires_in is the negated echo.
	NegativeLifetime Kind = "negative expires_in"
	// BadProfile answers 200 with the echo as issued_token_type and
	// token_type.
	BadProfile Kind = "echoed token profile"
	// OAuthCode answers 400 with the echo as the OAuth error code.
	OAuthCode Kind = "echoed OAuth error code"
	// OAuthDescription answers 401 invalid_client with the echo in the
	// error_description.
	OAuthDescription Kind = "echoed error_description"
	// TerminalDescription answers 400 invalid_grant with the echo in the
	// error_description.
	TerminalDescription Kind = "echoed invalid_grant description"
	// ServerError answers 500 with the echo as the body.
	ServerError Kind = "echoed 500 body"
	// RateLimited answers 429 with the echo as Retry-After and the body.
	RateLimited Kind = "echoed 429"
)

// Kinds is every hostile answer, for table tests.
var Kinds = []Kind{
	MalformedLocation, Redirect, MalformedStatus, MalformedHeader, MalformedTrailer,
	HugeNumber, NegativeLifetime, BadProfile, OAuthCode, OAuthDescription,
	TerminalDescription, ServerError, RateLimited,
}

const accessTokenURN = "urn:ietf:params:oauth:token-type:access_token" // #nosec G101 -- RFC 8693 URN

// Handler serves kind, reflecting the request's credentials.
func Handler(kind Kind) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echo, number := Echo(r), EchoNumber(r)
		switch kind {
		case MalformedLocation:
			w.Header().Set("Location", "/%zz/"+echo)
			w.WriteHeader(http.StatusFound)
		case Redirect:
			w.Header().Set("Location", "/elsewhere/"+echo)
			w.WriteHeader(http.StatusTemporaryRedirect)
		case MalformedStatus:
			raw(w, "HTTP/1.1 "+echo+"\r\nContent-Length: 0\r\n\r\n")
		case MalformedHeader:
			raw(w, "HTTP/1.1 200 OK\r\n"+echo+"\r\nContent-Length: 0\r\n\r\n")
		case MalformedTrailer:
			raw(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"+
				"2\r\n{}\r\n0\r\n"+echo+"\r\n\r\n")
		case HugeNumber:
			writeJSON(w, http.StatusOK, fmt.Sprintf(
				`{"access_token":"bc_at_minted","token_type":"Bearer","issued_token_type":%q,"expires_in":%s,"identity":{"id":%s}}`,
				accessTokenURN, number, number))
		case NegativeLifetime:
			writeJSON(w, http.StatusOK, fmt.Sprintf(
				`{"access_token":"bc_at_minted","token_type":"Bearer","issued_token_type":%q,"expires_in":-%s,"identity":{"id":-%s}}`,
				accessTokenURN, number, number))
		case BadProfile:
			writeJSON(w, http.StatusOK, fmt.Sprintf(
				`{"access_token":"bc_at_minted","token_type":%q,"issued_token_type":%q,"identity":{"first_name":%q}}`,
				echo, echo, echo))
		case OAuthCode:
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf(`{"error":%q}`, echo))
		case OAuthDescription:
			writeJSON(w, http.StatusUnauthorized, fmt.Sprintf(
				`{"error":"invalid_client","error_description":"client rejected; request was %s"}`, echo))
		case TerminalDescription:
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf(
				`{"error":"invalid_grant","error_description":"grant rejected; request was %s"}`, echo))
		case ServerError:
			writeText(w, http.StatusInternalServerError, echo)
		case RateLimited:
			w.Header().Set("Retry-After", echo)
			writeText(w, http.StatusTooManyRequests, echo)
		default:
			panic("upstreamtest: unknown kind " + string(kind))
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body)) // #nosec G705 -- a deliberately reflecting fake endpoint
}

// writeText writes a plain-text body, typed so no client renders the echo
// as HTML.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body)) // #nosec G705 -- a deliberately reflecting fake endpoint
}

// raw writes a response the server would refuse to produce itself.
func raw(w http.ResponseWriter, response string) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic("upstreamtest: hijack: " + err.Error())
	}
	defer func() { _ = conn.Close() }()
	_, _ = buf.WriteString(response)
	_ = buf.Flush()
}

// WindowBytes is the length of the substrings AssertClean looks for. A sink
// holding a 12-byte piece of a credential leaks it as surely as one holding
// all of it, since truncation is one of the ways text reaches a sink.
const WindowBytes = 12

// AssertClean fails t when text, from the named sink, contains any
// WindowBytes-long piece of any secret.
func AssertClean(t testing.TB, sink, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if len(s) < WindowBytes {
			if strings.Contains(text, s) {
				t.Errorf("%s contains a planted credential (%d bytes)", sink, len(s))
			}
			continue
		}
		for i := 0; i+WindowBytes <= len(s); i++ {
			if strings.Contains(text, s[i:i+WindowBytes]) {
				t.Errorf("%s contains bytes %d-%d of a planted credential:\n%s", sink, i, i+WindowBytes, text)
				break
			}
		}
	}
}
