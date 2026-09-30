// Package cassette records and replays a product API's HTTP exchanges so a
// real MCP server can run hermetically: the server is pointed at a local
// Player (its API base URL), which answers from recorded interactions instead
// of the product backend. The server's own code — SDK client, pagination,
// error masking — runs unchanged; only the network is replaced.
//
// It is product-agnostic by construction: a cassette is plain method + path +
// query → status + headers + JSON body, so it never imports a product SDK.
//
// Recording (Recorder) proxies to a live API under an explicit test profile,
// injects the profile's token itself (the server under test only ever holds a
// dummy), refuses accounts the profile does not list, and scrubs what it
// stores: no request headers at all, an allowlist of response headers, the
// upstream origin rewritten to a placeholder, emails and avatars replaced, and
// profile-listed literals redacted.
package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// BasePlaceholder stands for the API origin inside a stored cassette. The
// Player substitutes its own URL on replay, so absolute URLs in recorded
// bodies (pagination Link headers, "url" fields the server follows) resolve
// back to the Player rather than the live API.
const BasePlaceholder = "{{base}}"

// Cassette is a named, ordered set of recorded interactions.
type Cassette struct {
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	Interactions []Interaction `json:"interactions"`
}

// Interaction is one request pattern and the response served for it.
//
// After is the backend state the response belongs to: the writes ("METHOD
// /path") that had landed before it was recorded. A read recorded before and
// after `POST /x` is two interactions, the second with After ["POST /x"],
// and the Player serves a response only once its own replay has landed every
// write in its After (a write named twice must land twice) — so state
// follows the agent's own writes: re-reading
// never advances it, an unrelated write never unlocks it, and a resource
// first seen after its creation is a miss until the replay creates it.
type Interaction struct {
	Request  Request  `json:"request"`
	Response Response `json:"response"`
	After    []string `json:"after,omitempty"`
}

// stateKey identifies an interaction for dedup and merge: its pattern, its
// backend state, and — for a write — the body sent, so a rejected write and
// its corrected retry (same path, no landed write between) stay distinct.
func (in Interaction) stateKey() string {
	after := append([]string(nil), in.After...)
	sort.Strings(after)
	k := patternKey(in.Request) + "\x00" + strings.Join(after, "\x00")
	if in.Request.Method != "GET" && in.Request.Method != "HEAD" {
		k += "\x00" + canonicalJSON(in.Request.Body) // key order and indentation must not split a state
	}
	return k
}

// decodedMatches reports whether any string in a JSON body, decoded (so
// \u0040 is @ and \u002e is .), matches re.
func decodedMatches(body json.RawMessage, re *regexp.Regexp) bool {
	if len(body) == 0 {
		return false
	}
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case string:
			return re.MatchString(t)
		case map[string]any:
			for k, c := range t {
				if re.MatchString(k) || walk(c) {
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

// validHeaderValue reports whether net/http can send v as a field value: no
// control characters but tab (RFC 9110 field-value).
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// Landed reports whether a write answered with status actually happened:
// a 2xx. A redirect or an error changed nothing the replay can count on.
func Landed(status int) bool { return status >= 200 && status < 300 }

// toPlaceholder rewrites an origin to BasePlaceholder in a body, in both the
// plain and the slash-escaped JSON spelling (http:\/\/127.0.0.1:…).
func toPlaceholder(body []byte, origin string) []byte {
	if origin == "" {
		return body
	}
	body = bytes.ReplaceAll(body, []byte(origin), []byte(BasePlaceholder))
	return bytes.ReplaceAll(body, []byte(strings.ReplaceAll(origin, "/", `\/`)), []byte(BasePlaceholder))
}

// writeKey is how a landed write is named in After: method and path, the
// ".json" suffix dropped, so both spellings name one write.
func writeKey(method, path string) string {
	return method + " " + strings.TrimSuffix(path, ".json")
}

// Request identifies which requests an interaction answers. Method and Path
// must match exactly. Query is a subset match: every listed parameter must be
// present with that value, and parameters the request carries beyond them are
// ignored (so a server adding page=1 or a default filter still replays). When
// several interactions match, the one naming the most query parameters wins.
//
// Body is what the recorded request sent. It is kept for reading and for
// authoring expectations, never matched: a write is answered by path, and what
// the agent actually sent is asserted from the Player's exchange log.
type Request struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
}

// Response is what the Player serves. Body is JSON; BodyText carries a
// non-JSON body verbatim. Both have BasePlaceholder substituted on replay, as
// do header values.
type Response struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	BodyText string            `json:"body_text,omitempty"`
}

// Load reads and validates a cassette file.
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cassette
	if err := DecodeStrict(data, &c); err != nil {
		return nil, fmt.Errorf("cassette %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("cassette %s: %w", path, err)
	}
	return &c, nil
}

// Validate rejects a cassette the Player could not serve faithfully.
func (c *Cassette) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("cassette has no name")
	}
	states := map[string]int{}
	for i, in := range c.Interactions {
		r := in.Request
		if prev, dup := states[in.stateKey()]; dup {
			return fmt.Errorf("interaction #%d (%s %s) repeats #%d's request and state: the Player could never serve it", i+1, r.Method, r.Path, prev)
		}
		states[in.stateKey()] = i + 1
		switch r.Method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
		default:
			return fmt.Errorf("interaction #%d: unsupported method %q", i+1, r.Method)
		}
		if !strings.HasPrefix(r.Path, "/") || strings.ContainsAny(r.Path, "?#") {
			return fmt.Errorf("interaction #%d: path %q must be absolute and carry no query (use request.query) or fragment", i+1, r.Path)
		}
		// Terminal statuses only: a 1xx is informational, and the Player
		// would end up sending an implicit 200 after it.
		if in.Response.Status < 200 || in.Response.Status > 599 {
			return fmt.Errorf("interaction #%d (%s %s): response status %d", i+1, r.Method, r.Path, in.Response.Status)
		}
		if strings.Contains(string(in.Response.Body)+in.Response.BodyText, BasePlaceholder+"@") || decodedMatches(in.Response.Body, placeholderAuthority) || decodedMatches(json.RawMessage(in.Response.BodyText), placeholderAuthority) {
			return fmt.Errorf("interaction #%d (%s %s): %s@ in a body would make the Player's origin userinfo", i+1, r.Method, r.Path, BasePlaceholder)
		}
		if len(r.Body) > 0 && !json.Valid(r.Body) {
			return fmt.Errorf("interaction #%d (%s %s): request body is not JSON (the Player refuses non-JSON bodies)", i+1, r.Method, r.Path)
		}
		if placeholderAuthority.MatchString(string(in.Response.Body) + in.Response.BodyText) {
			return fmt.Errorf("interaction #%d (%s %s): %s runs on into a longer authority", i+1, r.Method, r.Path, BasePlaceholder)
		}
		if len(in.Response.Body) > 0 && !json.Valid(in.Response.Body) {
			return fmt.Errorf("interaction #%d (%s %s): response body is not JSON (use body_text)", i+1, r.Method, r.Path)
		}
		for _, w := range in.After {
			m, p, ok := strings.Cut(w, " ")
			switch m {
			case "POST", "PUT", "PATCH", "DELETE":
			default:
				ok = false
			}
			if !ok || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?# ") {
				return fmt.Errorf("interaction #%d (%s %s): after entry %q must be a write, \"METHOD /path\"", i+1, r.Method, r.Path, w)
			}
		}
		if (len(in.Response.Body) > 0 || in.Response.BodyText != "") && (r.Method == "HEAD" || in.Response.Status == 204 || in.Response.Status == 304) {
			return fmt.Errorf("interaction #%d (%s %s): a %d answer to %s carries no body", i+1, r.Method, r.Path, in.Response.Status, r.Method)
		}
		if len(in.Response.Body) > 0 && in.Response.BodyText != "" {
			return fmt.Errorf("interaction #%d (%s %s): both body and body_text set", i+1, r.Method, r.Path)
		}
		canon := map[string]bool{}
		for k, v := range in.Response.Headers {
			if canon[http.CanonicalHeaderKey(k)] {
				return fmt.Errorf("interaction #%d (%s %s): header %q appears twice (header names are case-insensitive)", i+1, r.Method, r.Path, k)
			}
			canon[http.CanonicalHeaderKey(k)] = true
			if !validHeaderValue(v) {
				return fmt.Errorf("interaction #%d (%s %s): header %q has a value net/http cannot send", i+1, r.Method, r.Path, k)
			}
			// The recorder's allowlist, for hand-authored cassettes too:
			// framing headers (Content-Length) are the transport's to set.
			if !slices.Contains(keptResponseHeaders, http.CanonicalHeaderKey(k)) {
				return fmt.Errorf("interaction #%d (%s %s): header %q is not one a cassette may carry (%s)", i+1, r.Method, r.Path, k, strings.Join(keptResponseHeaders, ", "))
			}
			// A followable URL must stay on the Player: relative, or {{base}}.
			if h := http.CanonicalHeaderKey(k); h == "Location" || h == "Link" {
				targets := []string{v}
				if h == "Link" {
					targets = nil
					for _, m := range linkTarget.FindAllStringSubmatch(v, -1) {
						targets = append(targets, m[1])
					}
				}
				for _, t := range targets {
					// Parse as the Player will serve it, a real origin in
					// place of {{base}}: the target must resolve to that host.
					const probe = "http://player.invalid"
					u, err := url.Parse(strings.ReplaceAll(strings.TrimSpace(t), BasePlaceholder, probe))
					if err != nil || u.User != nil || (u.Host != "" && u.Scheme+"://"+u.Host != probe) || (u.Host == "" && u.Scheme != "") {
						return fmt.Errorf("interaction #%d (%s %s): %s target %q leaves the Player (use a relative path or %s)", i+1, r.Method, r.Path, h, t, BasePlaceholder)
					}
				}
			}
		}
	}
	return nil
}

// ValidateLayers checks what a single cassette cannot: that every state a
// layered set of cassettes names in after is reachable — some layer answers
// that write with a success — so a replay never waits on a write it can't land.
func ValidateLayers(cassettes ...*Cassette) error {
	// Reachability as a fixed point: a write lands once some successful
	// interaction answering it is itself reachable — so a write that waits
	// on itself, or two that wait on each other, never land.
	type write struct {
		key   string
		after []string
	}
	// Later layers shadow earlier ones, as in the Player: a write whose
	// request and state a later layer answers counts only as that layer
	// answers it.
	// The Player ranks layer above body, so a later layer's answer for a
	// request and state shadows every body an earlier layer recorded for it.
	var writes []write
	shadowed := map[string]bool{}
	for li := len(cassettes) - 1; li >= 0; li-- {
		var inLayer []string
		for _, in := range cassettes[li].Interactions {
			if in.Request.Method == "GET" || in.Request.Method == "HEAD" {
				continue
			}
			after := append([]string(nil), in.After...)
			sort.Strings(after)
			k := patternKey(in.Request) + "\x00" + strings.Join(after, "\x00")
			if shadowed[k] {
				continue
			}
			inLayer = append(inLayer, k)
			if Landed(in.Response.Status) {
				writes = append(writes, write{writeKey(in.Request.Method, in.Request.Path), in.After})
			}
		}
		for _, k := range inLayer {
			shadowed[k] = true
		}
	}
	landed := map[string]int{}
	reached := func(after []string) bool {
		need := map[string]int{}
		for _, w := range after {
			m, path, _ := strings.Cut(w, " ")
			need[writeKey(m, path)]++
		}
		for k, n := range need {
			if landed[k] < n {
				return false
			}
		}
		return true
	}
	done := make([]bool, len(writes))
	for progress := true; progress; {
		progress = false
		for i, w := range writes {
			if !done[i] && reached(w.after) {
				done[i], progress = true, true
				landed[w.key]++
			}
		}
	}
	for _, c := range cassettes {
		for _, in := range c.Interactions {
			if !reached(in.After) {
				return fmt.Errorf("cassette %s: %s %s waits on %v, which the layers can never land", c.Name, in.Request.Method, in.Request.Path, in.After)
			}
		}
	}
	return nil
}

// DecodeStrict decodes exactly one JSON document into v: unknown fields are
// an error (a typo'd key must not silently drop a setting), and so is
// anything after the document (an append-edited or concatenated file must not
// run on its stale first half).
func DecodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return fmt.Errorf("unexpected data after the JSON document")
	}
	return nil
}

// Save writes the cassette as indented JSON, atomically: a failed write
// leaves any existing cassette at path intact.
func (c *Cassette) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cassette-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// queryMatches reports whether every parameter the pattern names is present in
// the request with that value.
func queryMatches(pattern map[string]string, got url.Values) bool {
	for k, v := range pattern {
		// Presence first: Get returns "" for an absent key too, which would
		// let {"archived": ""} match a request that never sent it.
		if _, ok := got[k]; !ok || got.Get(k) != v {
			return false
		}
	}
	return true
}

// canonicalQuery renders a query with sorted keys, so logged exchanges are
// stable regardless of the order a client encoded them in.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}
