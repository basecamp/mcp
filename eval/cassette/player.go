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
// the Player serves the latest state its replay has reached: of the
// interactions whose After writes have all landed, the one requiring most.
type Player struct {
	mu      sync.Mutex
	entries []entry
	landed  map[string]int // writeKey -> occurrences landed so far
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
	p := &Player{landed: map[string]int{}}
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
	body, readErr := io.ReadAll(r.Body)
	ex := Exchange{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  canonicalQuery(r.URL.Query()),
		Body:   compactJSON(body),
	}

	if readErr != nil {
		// A request cut off in transit did not arrive: unmatched, and it
		// neither lands a write nor advances state.
		ex.Status = http.StatusBadRequest
		p.mu.Lock()
		p.log = append(p.log, ex)
		p.mu.Unlock()
		http.Error(w, `{"error":"request body truncated"}`, http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	// Bodies are compared with this Player's origin as {{base}}, as the
	// recorder stores them: a URL the server copied from an answer into a
	// write carries a per-run origin.
	idx := p.match(r, string(toPlaceholder([]byte(ex.Body), p.base)))
	var resp Response
	if idx >= 0 {
		resp = p.entries[idx].in.Response
		ex.Matched = true
	} else {
		resp = Response{Status: http.StatusNotFound, Body: json.RawMessage(`{"status":404,"error":"Not Found"}`)}
	}
	ex.Status = resp.Status
	if ex.Matched && ex.IsWrite() && Landed(ex.Status) {
		p.landed[writeKey(ex.Method, ex.Path)]++
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

// match picks the interaction for r, or -1. Only interactions whose After
// writes have all landed are eligible. Most query parameters named wins;
// then the latest layer; then the latest state (most After writes); then, for
// a write, the interaction recorded with the same body (a rejected attempt
// and its corrected retry answer differently).
// Caller holds p.mu.
func (p *Player) match(r *http.Request, body string) int {
	q := r.URL.Query()
	best := -1
	for i, e := range p.entries {
		req := e.in.Request
		if req.Method != r.Method || !samePath(req.Path, r.URL.Path) || !queryMatches(req.Query, q) || !p.reached(e.in) {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := p.entries[best]
		switch {
		case len(req.Query) != len(b.in.Request.Query):
			if len(req.Query) > len(b.in.Request.Query) {
				best = i
			}
		case e.layer != b.layer:
			if e.layer > b.layer {
				best = i
			}
		case len(e.in.After) != len(b.in.After):
			if len(e.in.After) > len(b.in.After) {
				best = i
			}
		case compactJSON(e.in.Request.Body) == body && compactJSON(b.in.Request.Body) != body:
			best = i
		}
	}
	return best
}

// reached reports whether every write in the interaction's After has landed
// — as many times as After names it.
func (p *Player) reached(in Interaction) bool {
	need := map[string]int{}
	for _, w := range in.After {
		m, path, _ := strings.Cut(w, " ")
		need[writeKey(m, path)]++
	}
	for k, n := range need {
		if p.landed[k] < n {
			return false
		}
	}
	return true
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
