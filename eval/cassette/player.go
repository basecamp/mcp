package cassette

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Exchange is one request the Player received and what it answered. The log
// of exchanges is the replayed backend's final state: every write the agent's
// calls produced lands here with the body it sent, so a grader asserts "the
// comment was posted to the right recording and mentions Annie" against it
// rather than against the model's description of what it did.
type Exchange struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query,omitempty"`
	Body   string `json:"body,omitempty"`
	Status int    `json:"status"`
	// Matched is false when no interaction answered the request and the Player
	// served a 404. Against a cassette that covers the task's data, a miss is
	// a request for something the account does not have — a wrong or invented
	// id. Against a thin hand-authored cassette it can also be a legitimate
	// request nobody recorded, which is why misses are reported by path.
	Matched bool `json:"matched"`
}

// IsWrite reports whether the exchange mutates backend state.
func (e Exchange) IsWrite() bool { return e.Method != http.MethodGet && e.Method != http.MethodHead }

// Line renders the exchange as one matchable line: METHOD path?query body.
func (e Exchange) Line() string {
	var b strings.Builder
	b.WriteString(e.Method)
	b.WriteByte(' ')
	b.WriteString(e.Path)
	if e.Query != "" {
		b.WriteByte('?')
		b.WriteString(e.Query)
	}
	if e.Body != "" {
		b.WriteByte(' ')
		b.WriteString(e.Body)
	}
	return b.String()
}

// Player serves recorded interactions over HTTP. Later cassettes take
// precedence over earlier ones for the same request (a task cassette layered
// on a shared base overrides it). Among identical patterns in one cassette,
// the Player serves the state its replay has reached: the interaction with
// the highest AfterWrites not above the writes landed so far.
type Player struct {
	mu      sync.Mutex
	entries []entry
	writes  int // writes landed (answered below 400) so far
	log     []Exchange
	base    string
	srv     *httptest.Server
}

type entry struct {
	layer int
	in    Interaction
}

// NewPlayer builds a Player over the cassettes, in precedence order (last
// wins).
func NewPlayer(cassettes ...*Cassette) *Player {
	p := &Player{}
	for layer, c := range cassettes {
		for _, in := range c.Interactions {
			p.entries = append(p.entries, entry{layer: layer, in: in})
		}
	}
	return p
}

// Start serves the Player on a loopback listener and returns its base URL.
func (p *Player) Start() string {
	p.srv = httptest.NewServer(p)
	p.mu.Lock()
	p.base = p.srv.URL
	p.mu.Unlock()
	return p.srv.URL
}

// Close stops a started Player.
func (p *Player) Close() {
	if p.srv != nil {
		p.srv.Close()
	}
}

// SetBase sets the URL substituted for BasePlaceholder when the Player is
// mounted by the caller rather than started with Start.
func (p *Player) SetBase(base string) {
	p.mu.Lock()
	p.base = base
	p.mu.Unlock()
}

// Log returns a copy of every exchange so far.
func (p *Player) Log() []Exchange {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Exchange(nil), p.log...)
}

// Len returns the number of exchanges so far, a mark for Since.
func (p *Player) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.log)
}

// Since returns the exchanges after mark.
func (p *Player) Since(mark int) []Exchange {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mark >= len(p.log) {
		return nil
	}
	return append([]Exchange(nil), p.log[mark:]...)
}

func patternKey(r Request) string {
	return r.Method + " " + strings.TrimSuffix(r.Path, ".json") + "?" + canonicalQuery(toValues(r.Query))
}

// samePath compares paths modulo a trailing ".json": the API serves both
// spellings of a resource, and SDK versions differ in which they request, so
// a cassette recorded with one still answers the other.
func samePath(a, b string) bool {
	return strings.TrimSuffix(a, ".json") == strings.TrimSuffix(b, ".json")
}

func toValues(m map[string]string) map[string][]string {
	v := map[string][]string{}
	for k, s := range m {
		v[k] = []string{s}
	}
	return v
}

// ServeHTTP answers one request from the cassettes, logging it either way.
func (p *Player) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ex := Exchange{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  canonicalQuery(r.URL.Query()),
		Body:   compactJSON(body),
	}

	p.mu.Lock()
	idx := p.match(r)
	var resp Response
	if idx >= 0 {
		resp = p.entries[idx].in.Response
		ex.Matched = true
	} else {
		resp = Response{Status: http.StatusNotFound, Body: json.RawMessage(`{"status":404,"error":"Not Found"}`)}
	}
	ex.Status = resp.Status
	if ex.Matched && ex.IsWrite() && ex.Status < 400 {
		p.writes++
	}
	p.log = append(p.log, ex)
	base := p.base
	p.mu.Unlock()

	for k, v := range resp.Headers {
		w.Header().Set(k, strings.ReplaceAll(v, BasePlaceholder, base))
	}
	var out []byte
	switch {
	case len(resp.Body) > 0:
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		out = bytes.ReplaceAll(resp.Body, []byte(BasePlaceholder), []byte(base))
	case resp.BodyText != "":
		out = []byte(strings.ReplaceAll(resp.BodyText, BasePlaceholder, base))
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(out)
}

// match picks the interaction for r, or -1. Most query parameters named wins;
// then the latest layer; within that layer's identical patterns, the state
// the replay has reached (the earliest state when it has reached none).
// Caller holds p.mu.
func (p *Player) match(r *http.Request) int {
	q := r.URL.Query()
	best, bestSpec, bestLayer := -1, -1, -1
	for i, e := range p.entries {
		req := e.in.Request
		if req.Method != r.Method || !samePath(req.Path, r.URL.Path) || !queryMatches(req.Query, q) {
			continue
		}
		spec := len(req.Query)
		if spec > bestSpec || (spec == bestSpec && e.layer > bestLayer) {
			best, bestSpec, bestLayer = i, spec, e.layer
		}
	}
	if best < 0 {
		return -1
	}
	key := patternKey(p.entries[best].in.Request)
	pick, earliest := -1, -1
	for i, e := range p.entries {
		if e.layer != bestLayer || patternKey(e.in.Request) != key {
			continue
		}
		aw := e.in.AfterWrites
		if earliest < 0 || aw < p.entries[earliest].in.AfterWrites {
			earliest = i
		}
		if aw <= p.writes && (pick < 0 || aw > p.entries[pick].in.AfterWrites) {
			pick = i
		}
	}
	if pick < 0 {
		return earliest
	}
	return pick
}

// compactJSON renders a JSON body on one line; a non-JSON body is kept as-is.
func compactJSON(b []byte) string {
	if len(bytes.TrimSpace(b)) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}
