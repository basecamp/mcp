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
	"net/url"
	"os"
	"path/filepath"
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
		k += "\x00" + compactJSON(in.Request.Body) // indentation from a saved file must not split a state
	}
	return k
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
	for i, in := range c.Interactions {
		r := in.Request
		switch r.Method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
		default:
			return fmt.Errorf("interaction #%d: unsupported method %q", i+1, r.Method)
		}
		if !strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "?") {
			return fmt.Errorf("interaction #%d: path %q must be absolute and carry no query (use request.query)", i+1, r.Path)
		}
		if in.Response.Status < 100 || in.Response.Status > 599 {
			return fmt.Errorf("interaction #%d (%s %s): response status %d", i+1, r.Method, r.Path, in.Response.Status)
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
		if len(in.Response.Body) > 0 && in.Response.BodyText != "" {
			return fmt.Errorf("interaction #%d (%s %s): both body and body_text set", i+1, r.Method, r.Path)
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
