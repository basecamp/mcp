package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Profile is the explicit statement of what a recording may touch. Recording
// proxies live API traffic, so it is refused without one, and a profile must
// say in so many words that its account is seeded test data: production
// accounts hold real people's work, which never belongs in a committed
// fixture.
type Profile struct {
	// Name labels the profile in errors and cassette descriptions.
	Name string `json:"name"`
	// TestAccount must be true: the author asserts every account listed is a
	// seeded test account, not production data.
	TestAccount bool `json:"test_account"`
	// Upstream is the live API origin, e.g. https://3.basecampapi.com.
	Upstream string `json:"upstream"`
	// AccountIDs are the only accounts the recorder forwards to. A request
	// whose first path segment is a numeric id outside this list is refused
	// locally and never reaches the upstream.
	AccountIDs []string `json:"account_ids"`
	// TokenEnv names the environment variable holding the test account's
	// bearer token. The recorder injects it upstream itself; the server under
	// test holds only a dummy, and the token is never written anywhere.
	TokenEnv string `json:"token_env"`
	// Redact maps literal strings in recorded bodies to their replacements —
	// real names of the test account's people, a company name — applied
	// after the built-in email and avatar scrubbing.
	Redact map[string]string `json:"redact,omitempty"`
}

// LoadProfile reads and validates a recording profile.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := DecodeStrict(data, &p); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	return &p, nil
}

var numericSegment = regexp.MustCompile(`^[0-9]+$`)

// Validate refuses a profile that could record something it should not.
func (p *Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("profile has no name")
	}
	if !p.TestAccount {
		return fmt.Errorf("profile %q does not declare test_account: true — recording is only for seeded test accounts, never production data", p.Name)
	}
	u, err := url.Parse(p.Upstream)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil || defaultPort(u.Port()) {
		return fmt.Errorf("profile %q: upstream %q must be a bare https origin", p.Name, p.Upstream)
	}
	if len(p.AccountIDs) == 0 {
		return fmt.Errorf("profile %q lists no account_ids: the recorder forwards only to named test accounts", p.Name)
	}
	for _, id := range p.AccountIDs {
		if !numericSegment.MatchString(id) {
			return fmt.Errorf("profile %q: account id %q is not numeric", p.Name, id)
		}
	}
	if strings.TrimSpace(p.TokenEnv) == "" {
		return fmt.Errorf("profile %q names no token_env", p.Name)
	}
	for from, to := range p.Redact {
		// A literal that is itself a JSON scalar (123, true, null) would
		// rewrite values, not text, and could leave valid JSON with wrong
		// ids. Redactions are for names and other text.
		if json.Valid([]byte(strings.TrimSpace(from))) {
			return fmt.Errorf("profile %q: redact literal %q is a JSON value; redact text such as names, not ids or literals", p.Name, from)
		}
		if strings.ContainsAny(from+to, "\"\\") || strings.ContainsFunc(from+to, unicode.IsControl) {
			return fmt.Errorf("profile %q: redact literal %q -> %q carries a quote, backslash, or control character, which would corrupt the JSON it rewrites", p.Name, from, to)
		}
	}
	return nil
}

// Token reads the profile's token from its environment variable.
func (p *Profile) Token() (string, error) {
	t := os.Getenv(p.TokenEnv)
	if t == "" {
		return "", fmt.Errorf("profile %q: %s is not set", p.Name, p.TokenEnv)
	}
	return t, nil
}

// keptResponseHeaders are the only response headers a cassette stores. Every
// other header — cookies, request ids, rate-limit and cache state — is
// dropped: none affects how a client parses the answer, and some identify the
// session.
var keptResponseHeaders = []string{"Content-Type", "Link", "X-Total-Count", "Location"}

// Recorder proxies requests to a live test account and keeps a scrubbed copy
// of each exchange. It is an http.Handler; mount it with Start and point the
// server under test at its URL.
type Recorder struct {
	profile  *Profile
	token    string
	client   *http.Client
	scrubber *Scrubber

	mu           sync.Mutex
	interactions []Interaction
	log          []Exchange
	landed       []string // writeKey of each write landed upstream so far, one per occurrence
	faults       []string
	// serial makes each exchange — forward, record, land — one step, so a
	// concurrent request cannot record a post-write answer before that
	// write is counted landed. Recording is one episode; throughput is moot.
	serial sync.Mutex
	base   string
	srv    *httptest.Server
}

// NewRecorder builds a recorder for a validated profile. The token is read
// once, here, so a missing credential fails before any traffic.
func NewRecorder(p *Profile) (*Recorder, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	token, err := p.Token()
	if err != nil {
		return nil, err
	}
	return &Recorder{
		profile: p,
		token:   token,
		client: &http.Client{
			Timeout: 60 * time.Second,
			// Never follow a redirect upstream: the hop would carry the
			// injected token to a path the account allowlist never saw. The
			// redirect goes back to the server under test instead (its
			// Location rewritten to the recorder), so the next hop re-enters
			// ServeHTTP and is checked like any other request.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		// Email aliases are keyed by the token, so they are stable across
		// every episode and run recorded under one profile (merged cassettes
		// agree on who person-… is) without being reversible by anyone who
		// lacks the token.
		scrubber: NewScrubber(p.Upstream, withToken(p.Redact, token), token),
	}, nil
}

// withToken adds the credential itself to the redactions, so an upstream
// that echoes it (a body, a Location, a Link) never puts it in a cassette.
func withToken(redact map[string]string, token string) map[string]string {
	out := map[string]string{token: "[redacted-token]"}
	for k, v := range redact {
		out[k] = v
	}
	return out
}

// Start serves the recorder on loopback and returns its URL.
func (r *Recorder) Start() string {
	r.srv = httptest.NewServer(r)
	r.mu.Lock()
	r.base = r.srv.URL
	r.mu.Unlock()
	return r.srv.URL
}

// Close stops a started recorder.
func (r *Recorder) Close() {
	if r.srv != nil {
		r.srv.Close()
	}
}

// Cassette returns everything recorded so far as a cassette, one interaction
// per distinct request pattern (the first response recorded for it).
func (r *Recorder) Cassette(name, description string) *Cassette {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &Cassette{Name: name, Description: description, Interactions: append([]Interaction(nil), r.interactions...)}
}

// Len returns the number of exchanges proxied so far, a mark for Since.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.log)
}

// Since returns the exchanges proxied after mark — the same log a Player
// keeps, so a recording run is graded like a replayed one.
func (r *Recorder) Since(mark int) []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mark >= len(r.log) {
		return nil
	}
	return append([]Exchange(nil), r.log[mark:]...)
}

// scrubQuery scrubs decoded query keys and values — before any encoding, so
// a literal like "a@b.example" or "Real Person" is matched as written, not
// as a%40b.example or Real+Person.
func (r *Recorder) scrubQuery(q url.Values) url.Values {
	r.mu.Lock()
	base := r.base
	r.mu.Unlock()
	clean := func(v string) string {
		// A URL the server copied from an answer points at this run's
		// recorder; store it as {{base}}, as the Player will compare it.
		return string(toPlaceholder([]byte(r.scrubber.String(v)), base))
	}
	out := url.Values{}
	for k, vs := range q {
		for _, v := range vs {
			out.Add(clean(k), clean(v))
		}
	}
	return out
}

func (r *Recorder) pathHasAccount(path string) bool {
	return numericSegment.MatchString(strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0])
}

// foreignAccounts returns the ids in any "accounts": [{"id": …}] list of a
// JSON body that the profile does not name.
func foreignAccounts(body []byte, allowed []string) []string {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // account ids compared exactly, never through float64
	if dec.Decode(&doc) != nil {
		return nil
	}
	ok := map[string]bool{}
	for _, id := range allowed {
		ok[id] = true
	}
	var foreign []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if list, isList := child.([]any); k == "accounts" && isList {
					for _, item := range list {
						if m, isMap := item.(map[string]any); isMap {
							if id, has := m["id"]; has {
								if s := fmt.Sprint(id); !ok[s] {
									foreign = append(foreign, s)
								}
							}
						}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	return foreign
}

// allowed reports whether the request path may be forwarded: account-less
// paths (/authorization.json) and paths under a profile account.
func (r *Recorder) allowed(path string) bool {
	seg := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
	if !numericSegment.MatchString(seg) {
		return true
	}
	for _, id := range r.profile.AccountIDs {
		if seg == id {
			return true
		}
	}
	return false
}

// ServeHTTP forwards one request upstream with the profile token, records the
// scrubbed exchange, and answers the client with the live response (upstream
// origin rewritten to the recorder so follow-up requests stay proxied).
//
// Every anomaly — a refusal the recorder makes, or an upstream answer that is
// not a clean recording (a 5xx, a 429) — is a fault. A recording with any
// fault is incomplete: Faults reports them and the caller does not save it.
// One rule instead of a guard per failure mode: rerun on a reseeded account.
func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.serial.Lock()
	defer r.serial.Unlock()
	body, readErr := io.ReadAll(req.Body)
	r.mu.Lock()
	logBase := r.base
	r.mu.Unlock()
	ex := Exchange{Method: req.Method, Path: req.URL.Path, Query: canonicalQuery(r.scrubQuery(req.URL.Query())), Body: compactJSON(toPlaceholder(r.scrubber.Bytes(body), logBase))}
	refuse := func(status int, why string) {
		ex.Status = status
		r.appendLog(ex)
		r.fault(req, why)
		msg, _ := json.Marshal(map[string]string{"error": "recorder: " + why})
		http.Error(w, string(msg), status)
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		// A cassette cannot replay it, so it must not reach the live
		// account or the recording.
		refuse(http.StatusMethodNotAllowed, "method "+req.Method+" cannot be recorded")
		return
	}
	if readErr != nil {
		// A request cut off mid-body must not reach the live account.
		refuse(http.StatusBadRequest, "request body truncated")
		return
	}
	for k, vs := range req.URL.Query() {
		if len(vs) > 1 {
			// A cassette query is single-valued; recording this would
			// silently broaden what the pattern answers.
			refuse(http.StatusBadRequest, "query key "+r.scrubber.String(k)+" repeats; cassettes hold one value per key")
			return
		}
	}
	if len(bytes.TrimSpace(body)) > 0 && !json.Valid(body) {
		// A cassette keys writes by their JSON body; a form or multipart
		// body could not be told apart from its retry on replay.
		refuse(http.StatusUnsupportedMediaType, "non-JSON request body cannot be recorded")
		return
	}
	// Dot segments (literal or percent-encoded) could put a request under
	// another account once something upstream normalizes the path, past the
	// allowlist that read it un-normalized: refused outright.
	if hasDotSegment(req.URL.Path) || strings.Contains(strings.ToLower(req.URL.EscapedPath()), "%2e") {
		refuse(http.StatusBadRequest, "dot or empty segments in the path")
		return
	}
	if _, err := url.ParseQuery(req.URL.RawQuery); err != nil {
		// url.Query drops a malformed pair silently; the upstream would see
		// it and the cassette would not.
		refuse(http.StatusBadRequest, "malformed query string")
		return
	}
	if r.scrubber.String(req.URL.Path) != req.URL.Path {
		// A path carrying an email, the token, or a redacted name would be
		// stored verbatim (a path must replay as sent), so it is refused.
		refuse(http.StatusBadRequest, "the path carries data the scrubber would remove")
		return
	}
	if !r.allowed(req.URL.Path) {
		refuse(http.StatusForbidden, "account not in the test profile")
		return
	}
	// Checked before forwarding: a write must not reach the live account
	// only for its recording to be refused afterwards.
	if json.Valid(body) && !json.Valid(r.scrubber.Bytes(body)) {
		refuse(http.StatusBadGateway, "a profile redaction broke this request's JSON; redact text, not structure")
		return
	}
	if leaksPersonal(r.scrubber.Bytes(body)) || r.leaksOrigin(r.scrubber.Bytes(body)) {
		refuse(http.StatusBadRequest, "the request body carries personal data or the live origin in an encoded form")
		return
	}
	up, err := http.NewRequestWithContext(req.Context(), req.Method, strings.TrimSuffix(r.profile.Upstream, "/")+req.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		refuse(http.StatusBadGateway, r.scrubber.String(err.Error())) // may carry the live URL
		return
	}
	// Only the headers a JSON API call needs. The inbound Authorization (the
	// server's dummy) is never forwarded; the profile token replaces it.
	up.Header.Set("Authorization", "Bearer "+r.token)
	for _, h := range []string{"Content-Type", "Accept", "User-Agent"} {
		if v := req.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	resp, err := r.client.Do(up)
	if err != nil {
		refuse(http.StatusBadGateway, r.scrubber.String(err.Error()))
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		refuse(http.StatusBadGateway, "upstream body truncated")
		return
	}

	r.mu.Lock()
	base := r.base
	r.mu.Unlock()

	// Any URL the upstream hands back for the server to follow — a redirect,
	// a pagination link — must stay on the upstream origin: anywhere else (a
	// CDN, a signed URL) the recorder can neither see nor scrub.
	if bad := r.offOrigin(resp.Header); bad != "" {
		refuse(http.StatusBadGateway, bad+" points off the upstream origin")
		return
	}

	// An account-less answer (an identity document) may list every account
	// the token reaches. A token that reaches beyond the profile is refused:
	// a seeded test credential belongs to the test accounts only.
	if !r.pathHasAccount(req.URL.Path) {
		if foreign := foreignAccounts(respBody, r.profile.AccountIDs); len(foreign) > 0 {
			refuse(http.StatusForbidden, fmt.Sprintf("the token reaches %d account(s) outside the profile; record with a credential scoped to the test account", len(foreign)))
			return
		}
	}

	headers := map[string]string{}
	for _, h := range keptResponseHeaders {
		// Every field value, joined: an API may send prev and next as
		// separate Link fields, and Link's own syntax is comma-separated.
		if vs := resp.Header.Values(h); len(vs) > 0 {
			headers[h] = r.scrubber.String(strings.Join(vs, ", "))
		}
	}
	scrubbed := scrubAvatarFields(r.scrubber.Bytes(respBody))
	if !json.Valid(scrubbed) && !utf8.Valid(scrubbed) {
		// body_text is a JSON string: invalid UTF-8 would be replaced on
		// save and replay different bytes.
		refuse(http.StatusBadGateway, "a non-JSON response body is not valid UTF-8 and cannot be recorded")
		return
	}
	if bytes.Contains(scrubbed, []byte(BasePlaceholder+"@")) || bytes.Contains(scrubbed, []byte(strings.ReplaceAll(BasePlaceholder, "/", `\/`)+"@")) {
		// origin@host: replayed, the Player's origin would become userinfo
		// and host the real authority — a way out of the loopback.
		refuse(http.StatusBadGateway, "a response URL puts userinfo after the upstream origin")
		return
	}
	if r.leaksOrigin(scrubbed) {
		refuse(http.StatusBadGateway, "the upstream origin survives scrubbing in an encoded form")
		return
	}
	if leaksPersonal(scrubbed) {
		refuse(http.StatusBadGateway, "an email or avatar URL survives scrubbing in an encoded form")
		return
	}
	if json.Valid(respBody) && !json.Valid(scrubbed) {
		refuse(http.StatusBadGateway, "a profile redaction broke this response's JSON; redact text, not structure")
		return
	}
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout:
		// Transient: the server may retry and succeed, but a recording that
		// kept this answer would replay the failure forever.
		r.fault(req, fmt.Sprintf("upstream answered %d", resp.StatusCode))
	case resp.StatusCode == http.StatusUnauthorized:
		// An expired or wrong token records nothing but auth failures.
		r.fault(req, "upstream answered 401: check the profile token")
	}
	// A 404 is recorded too — it may override a lower layer's answer (an
	// item read after the task deleted it) — and the Player reports it as a
	// miss, so replayed or live, it counts as a wrong id.
	r.record(req, body, resp.StatusCode, headers, scrubbed)
	if req.Method != http.MethodGet && req.Method != http.MethodHead && Landed(resp.StatusCode) {
		// Every occurrence counts: two comments posted to one recording are
		// two writes, and a read after the second is a state of its own.
		r.mu.Lock()
		r.landed = append(r.landed, writeKey(req.Method, req.URL.Path))
		r.mu.Unlock()
	}
	ex.Status, ex.Matched = resp.StatusCode, resp.StatusCode != http.StatusNotFound
	r.appendLog(ex)

	// The live answer is the scrubbed one, {{base}} pointing back at the
	// recorder: the server under test — and so the episode's trace and the
	// results file — never sees what the cassette may not keep, and a
	// recording episode sees exactly what its replay will.
	for k, v := range headers {
		w.Header().Set(k, strings.ReplaceAll(v, BasePlaceholder, base))
	}
	// The Player's default for a JSON body with no Content-Type, applied
	// here too, so the recording episode sees what its replay will.
	if w.Header().Get("Content-Type") == "" && len(bytes.TrimSpace(scrubbed)) > 0 && json.Valid(scrubbed) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(bytes.ReplaceAll(scrubbed, []byte(BasePlaceholder), []byte(base)))
}

// linkTarget pulls each <URI> out of a Link header value.
var linkTarget = regexp.MustCompile(`<([^>]*)>`)

// offOrigin names the first followable URL (Location, or a Link target) that
// names a host other than the upstream's, or "" when all stay on it.
func (r *Recorder) offOrigin(h http.Header) string {
	origin := strings.TrimSuffix(r.profile.Upstream, "/")
	bad := func(raw string) bool {
		u, err := url.Parse(strings.TrimSpace(raw))
		return err != nil || u.User != nil || (u.Host != "" && (u.Scheme != "https" || "https://"+u.Host != origin))
	}
	if loc := h.Get("Location"); loc != "" && bad(loc) {
		return "a redirect"
	}
	for _, v := range h.Values("Link") {
		for _, m := range linkTarget.FindAllStringSubmatch(v, -1) {
			if bad(m[1]) {
				return "a Link target"
			}
		}
	}
	return ""
}

func (r *Recorder) fault(req *http.Request, why string) {
	r.mu.Lock()
	r.faults = append(r.faults, req.Method+" "+req.URL.Path+": "+why)
	r.mu.Unlock()
}

// leaksOrigin reports whether the live upstream origin (or host) survives
// in a scrubbed JSON body in any encoding — \u002f escapes and the like are
// decoded here, so a spelling the byte-level rewrite missed is caught.
func (r *Recorder) leaksOrigin(body []byte) bool {
	// Hostnames are case-insensitive: compare lowercased.
	host := strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(r.profile.Upstream, "/"), "https://"))
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		// Not JSON: the text itself, in any case.
		return strings.Contains(strings.ToLower(string(body)), host)
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case string:
			// Decoded, so \u0040 and friends are plain here: the origin
			// itself, or {{base}}@ (the replay origin turned userinfo).
			return strings.Contains(strings.ToLower(t), host) || strings.Contains(t, BasePlaceholder+"@")
		case map[string]any:
			for k, c := range t {
				if strings.Contains(strings.ToLower(k), host) || walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc)
}

// leaksPersonal reports whether a scrubbed JSON body still carries, once
// decoded (\u0040, \u005f and the like undone), an email address outside
// example.com or an avatar URL other than the placeholder: the byte-level
// scrubber missed a spelling, so the recording is refused, not saved.
func leaksPersonal(body []byte) bool {
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	var walk func(key string, v any) bool
	walk = func(key string, v any) bool {
		switch t := v.(type) {
		case string:
			if strings.HasPrefix(key, "avatar") && t != "" && t != "https://example.com/avatar.png" {
				return true
			}
			for _, m := range emailRE.FindAllString(t, -1) {
				if !strings.HasSuffix(strings.ToLower(m), "@example.com") {
					return true
				}
			}
		case map[string]any:
			for k, c := range t {
				if walk(k, c) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if walk(key, c) {
					return true
				}
			}
		}
		return false
	}
	return walk("", doc)
}

// defaultPort reports whether an explicit port is 443 in any spelling (0443
// included): the canonical origin omits it, so the scrubber would miss it.
func defaultPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n == 443
}

// hasDotSegment reports whether a path has a "." or ".." segment, or an
// empty one (//): anything a router upstream might normalize into another
// account's path after the allowlist has read it.
func hasDotSegment(p string) bool {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == "." || seg == ".." || (seg == "" && i > 0 && i < len(segs)-1) {
			return true
		}
	}
	return false
}

// Faults returns every anomaly seen so far. A recording with any is
// incomplete and must not be saved.
func (r *Recorder) Faults() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.faults...)
}

func (r *Recorder) appendLog(ex Exchange) {
	r.mu.Lock()
	r.log = append(r.log, ex)
	r.mu.Unlock()
}

// record stores one exchange; headers and respBody arrive scrubbed.
func (r *Recorder) record(req *http.Request, reqBody []byte, status int, headers map[string]string, respBody []byte) {
	q := map[string]string{}
	for k, v := range r.scrubQuery(req.URL.Query()) {
		if len(v) > 0 {
			q[k] = v[0]
		}
	}
	if len(q) == 0 {
		q = nil
	}
	in := Interaction{
		Request:  Request{Method: req.Method, Path: req.URL.Path, Query: q},
		Response: Response{Status: status, Headers: headers},
	}
	r.mu.Lock()
	base := r.base
	r.mu.Unlock()
	// A URL the server copied from an answer points at this run's recorder;
	// store it as {{base}}, as the Player will compare it.
	s := toPlaceholder(r.scrubber.Bytes(reqBody), base)
	if len(bytes.TrimSpace(s)) > 0 && json.Valid(s) {
		in.Request.Body = compactRaw(s)
	}
	if len(in.Response.Headers) == 0 {
		in.Response.Headers = nil
	}
	if len(bytes.TrimSpace(respBody)) > 0 {
		if json.Valid(respBody) {
			in.Response.Body = compactRaw(respBody)
		} else {
			in.Response.BodyText = string(respBody)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.landed) > 0 {
		in.After = append([]string(nil), r.landed...)
	}
	// Each pattern keeps one answer per backend state: the first seen after a
	// given set of landed writes. A read repeated with no write between adds
	// nothing; a read after a write is a new state the Player serves once its
	// own replay has landed those writes.
	key := in.stateKey()
	for _, prev := range r.interactions {
		if prev.stateKey() == key {
			return
		}
	}
	r.interactions = append(r.interactions, in)
}

func compactRaw(b []byte) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return json.RawMessage(b)
	}
	return json.RawMessage(buf.Bytes())
}

// Merge folds src's interactions into dst — so recording the same task under
// several models or arms accumulates the union of what they asked for. Per
// request pattern and backend state (After), dst's answer stands and src adds
// the states dst lacks.
func Merge(dst, src *Cassette) {
	have := map[string]bool{}
	for _, in := range dst.Interactions {
		have[in.stateKey()] = true
	}
	for _, in := range src.Interactions {
		if k := in.stateKey(); !have[k] {
			dst.Interactions = append(dst.Interactions, in)
			have[k] = true
		}
	}
	sort.SliceStable(dst.Interactions, func(i, j int) bool {
		a, b := dst.Interactions[i], dst.Interactions[j]
		if a.Request.Path != b.Request.Path {
			return a.Request.Path < b.Request.Path
		}
		return len(a.After) < len(b.After)
	})
}
