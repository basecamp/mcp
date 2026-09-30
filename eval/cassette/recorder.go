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
	"strings"
	"sync"
	"time"
	"unicode"
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
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
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
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
	writes       int // writes landed upstream so far
	base         string
	srv          *httptest.Server
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
		scrubber: NewScrubber(p.Upstream, p.Redact, token),
	}, nil
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
	out := url.Values{}
	for k, vs := range q {
		for _, v := range vs {
			out.Add(r.scrubber.String(k), r.scrubber.String(v))
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
	if json.Unmarshal(body, &doc) != nil {
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
								if s := fmt.Sprint(jsonNumber(id)); !ok[s] {
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

// jsonNumber renders a decoded JSON number as an integer when it is one.
func jsonNumber(v any) any {
	if f, isFloat := v.(float64); isFloat && f == float64(int64(f)) {
		return int64(f)
	}
	return v
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
func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	ex := Exchange{Method: req.Method, Path: req.URL.Path, Query: canonicalQuery(r.scrubQuery(req.URL.Query())), Body: compactJSON(r.scrubber.Bytes(body))}
	if !r.allowed(req.URL.Path) {
		ex.Status = http.StatusForbidden
		r.appendLog(ex)
		http.Error(w, `{"error":"recorder: account not in the test profile"}`, http.StatusForbidden)
		return
	}
	up, err := http.NewRequestWithContext(req.Context(), req.Method, strings.TrimSuffix(r.profile.Upstream, "/")+req.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		ex.Status = http.StatusBadGateway
		r.appendLog(ex)
		// The error text can carry the live URL; scrub it like any answer.
		http.Error(w, r.scrubber.String(err.Error()), http.StatusBadGateway)
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
		ex.Status = http.StatusBadGateway
		r.appendLog(ex)
		// The error text can carry the live URL; scrub it like any answer.
		http.Error(w, r.scrubber.String(err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		// A body cut off mid-read must not become a recorded answer.
		ex.Status = http.StatusBadGateway
		r.appendLog(ex)
		http.Error(w, r.scrubber.String("recorder: upstream body truncated: "+err.Error()), http.StatusBadGateway)
		return
	}

	r.mu.Lock()
	base := r.base
	r.mu.Unlock()

	// A redirect off the upstream origin would send the server under test
	// somewhere the recorder cannot see or scrub (a CDN, a signed URL); it is
	// refused rather than handed on or recorded.
	if loc := resp.Header.Get("Location"); loc != "" {
		// Any Location naming a host — absolute or network-path (//host/…) —
		// must name the upstream's own origin.
		if u, err := url.Parse(loc); err != nil || (u.Host != "" && (u.Scheme != "https" || "https://"+u.Host != strings.TrimSuffix(r.profile.Upstream, "/"))) {
			ex.Status = http.StatusBadGateway
			r.appendLog(ex)
			http.Error(w, `{"error":"recorder: upstream redirected off its origin"}`, http.StatusBadGateway)
			return
		}
	}

	// An account-less answer (an identity document) may list every account
	// the token reaches. A token that reaches beyond the profile is refused
	// outright rather than recorded: a seeded test credential belongs to the
	// test accounts only.
	if !r.pathHasAccount(req.URL.Path) {
		if foreign := foreignAccounts(respBody, r.profile.AccountIDs); len(foreign) > 0 {
			ex.Status = http.StatusForbidden
			r.appendLog(ex)
			http.Error(w, fmt.Sprintf(`{"error":"recorder: the token reaches %d account(s) outside the profile; record with a credential scoped to the test account"}`, len(foreign)), http.StatusForbidden)
			return
		}
	}

	headers := map[string]string{}
	for _, h := range keptResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			headers[h] = r.scrubber.String(v)
		}
	}
	scrubbed := r.scrubber.Bytes(respBody)
	// A 404 is not recorded: replayed, the same request stays a miss (and a
	// wrong id), exactly as it was live.
	if resp.StatusCode != http.StatusNotFound {
		r.record(req, body, resp.StatusCode, headers, scrubbed)
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead && resp.StatusCode < 400 {
		r.mu.Lock()
		r.writes++
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
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(bytes.ReplaceAll(scrubbed, []byte(BasePlaceholder), []byte(base)))
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
	if s := r.scrubber.Bytes(reqBody); len(bytes.TrimSpace(s)) > 0 && json.Valid(s) {
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
	in.AfterWrites = r.writes
	// Each pattern keeps one answer per backend state: the first seen after
	// a given number of writes. A read repeated with no write between adds
	// nothing; a read after a write is a new state the Player serves once
	// its own replay has landed as many writes.
	key := patternKey(in.Request)
	for _, prev := range r.interactions {
		if patternKey(prev.Request) == key && prev.AfterWrites == in.AfterWrites {
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
// request pattern and backend state (AfterWrites), dst's answer stands and
// src adds the states dst lacks.
func Merge(dst, src *Cassette) {
	type state struct {
		key    string
		writes int
	}
	have := map[state]bool{}
	for _, in := range dst.Interactions {
		have[state{patternKey(in.Request), in.AfterWrites}] = true
	}
	for _, in := range src.Interactions {
		k := state{patternKey(in.Request), in.AfterWrites}
		if !have[k] {
			dst.Interactions = append(dst.Interactions, in)
			have[k] = true
		}
	}
	sort.SliceStable(dst.Interactions, func(i, j int) bool {
		a, b := dst.Interactions[i], dst.Interactions[j]
		if a.Request.Path != b.Request.Path {
			return a.Request.Path < b.Request.Path
		}
		return a.AfterWrites < b.AfterWrites
	})
}
