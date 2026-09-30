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
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
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
		if strings.ContainsAny(from+to, "\"\\") {
			return fmt.Errorf("profile %q: redact literal %q -> %q carries a quote or backslash, which would corrupt the JSON it rewrites", p.Name, from, to)
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
		profile:  p,
		token:    token,
		client:   &http.Client{Timeout: 60 * time.Second},
		scrubber: NewScrubber(p.Upstream, p.Redact),
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
	ex := Exchange{Method: req.Method, Path: req.URL.Path, Query: canonicalQuery(req.URL.Query()), Body: compactJSON(r.scrubber.Bytes(body))}
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
		http.Error(w, err.Error(), http.StatusBadGateway)
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
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	r.mu.Lock()
	base := r.base
	r.mu.Unlock()

	headers := map[string]string{}
	for _, h := range keptResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	r.record(req, body, resp.StatusCode, headers, respBody)
	ex.Status, ex.Matched = resp.StatusCode, resp.StatusCode != http.StatusNotFound
	r.appendLog(ex)

	// Live answer: same response, upstream origin swapped for the recorder so
	// the server's next request (a Link page, a followed url) comes back here.
	for k, v := range headers {
		w.Header().Set(k, strings.ReplaceAll(v, r.profile.Upstream, base))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(bytes.ReplaceAll(respBody, []byte(r.profile.Upstream), []byte(base)))
}

func (r *Recorder) appendLog(ex Exchange) {
	r.mu.Lock()
	r.log = append(r.log, ex)
	r.mu.Unlock()
}

func (r *Recorder) record(req *http.Request, reqBody []byte, status int, headers map[string]string, respBody []byte) {
	q := map[string]string{}
	for k, v := range req.URL.Query() {
		if len(v) > 0 {
			q[k] = v[0]
		}
	}
	if len(q) == 0 {
		q = nil
	}
	in := Interaction{
		Request: Request{Method: req.Method, Path: req.URL.Path, Query: q},
		Response: Response{
			Status:  status,
			Headers: map[string]string{},
		},
	}
	if s := r.scrubber.Bytes(reqBody); len(bytes.TrimSpace(s)) > 0 && json.Valid(s) {
		in.Request.Body = compactRaw(s)
	}
	for k, v := range headers {
		in.Response.Headers[k] = r.scrubber.String(v)
	}
	if len(in.Response.Headers) == 0 {
		in.Response.Headers = nil
	}
	scrubbed := r.scrubber.Bytes(respBody)
	if len(bytes.TrimSpace(scrubbed)) > 0 {
		if json.Valid(scrubbed) {
			in.Response.Body = compactRaw(scrubbed)
		} else {
			in.Response.BodyText = string(scrubbed)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	key := patternKey(in.Request)
	for _, prev := range r.interactions {
		if patternKey(prev.Request) == key && req.Method == http.MethodGet {
			return // first read wins; a repeated GET adds nothing
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

// Merge folds src's interactions into dst, skipping request patterns dst
// already answers — so recording the same task under several models or arms
// accumulates the union of what they asked for.
func Merge(dst, src *Cassette) {
	have := map[string]bool{}
	for _, in := range dst.Interactions {
		have[patternKey(in.Request)] = true
	}
	for _, in := range src.Interactions {
		if k := patternKey(in.Request); !have[k] {
			dst.Interactions = append(dst.Interactions, in)
			have[k] = true
		}
	}
	sort.SliceStable(dst.Interactions, func(i, j int) bool {
		return dst.Interactions[i].Request.Path < dst.Interactions[j].Request.Path
	})
}
