package cassette

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Scrubber removes what a cassette must not carry: the live API origin
// (replaced with BasePlaceholder), email addresses (replaced with stable
// keyed per-address aliases, so one person stays one person across a cassette),
// avatar URLs (which embed signed CDN tokens), and caller-listed literals.
// It works on raw text as well as JSON so request bodies, response bodies and
// header values are all covered by the same rules.
type Scrubber struct {
	upstream string
	redact   [][2]string // longest first, so a full name replaces before a first name

	key []byte
}

var (
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	avatarRE = regexp.MustCompile(`"(avatar_url|avatar_url_large|avatar_thumbnail_url)"\s*:\s*"[^"]*"`)
)

// NewScrubber builds a scrubber for an upstream origin and literal
// redactions. key seeds the email aliases: the same key maps an address to
// the same alias every time, and without the key an alias cannot be walked
// back to its address.
func NewScrubber(upstream string, redact map[string]string, key string) *Scrubber {
	s := &Scrubber{upstream: strings.TrimSuffix(upstream, "/"), key: []byte(key)}
	for from, to := range redact {
		if from != "" {
			s.redact = append(s.redact, [2]string{from, to})
		}
	}
	sort.Slice(s.redact, func(i, j int) bool {
		if len(s.redact[i][0]) != len(s.redact[j][0]) {
			return len(s.redact[i][0]) > len(s.redact[j][0])
		}
		return s.redact[i][0] < s.redact[j][0]
	})
	return s
}

// String scrubs one string.
func (s *Scrubber) String(in string) string {
	return string(s.Bytes([]byte(in)))
}

// Bytes scrubs a body. JSON stays valid: every replacement swaps text inside
// a string literal for text without quotes or backslashes (Profile.Validate
// refuses redact literals that carry either).
func (s *Scrubber) Bytes(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	out := in
	if s.upstream != "" {
		// Case-insensitively (hosts are), in the plain spelling and with the
		// slashes a JSON encoder may escape.
		forms := []string{s.upstream}
		if u, err := url.Parse(s.upstream); err == nil {
			// The network-path spelling (//host/…) names the same origin;
			// the longer https:// form is replaced first, so this only
			// catches the bare reference.
			forms = append(forms, "//"+u.Host)
			if u.Port() == "" {
				forms = append(forms, s.upstream+":443", "//"+u.Host+":443") // default port spelled out
			}
		}
		for _, f := range append([]string(nil), forms...) {
			forms = append(forms, strings.ReplaceAll(f, "/", `\/`))
		}
		// Longest first, so the :443 spelling is not half-rewritten.
		sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
		for _, form := range forms {
			out = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(form)).ReplaceAll(out, []byte(BasePlaceholder))
		}
	}
	out = avatarRE.ReplaceAllFunc(out, func(m []byte) []byte {
		key := avatarRE.FindSubmatch(m)[1]
		return []byte(fmt.Sprintf(`"%s":"https://example.com/avatar.png"`, key))
	})
	out = emailRE.ReplaceAllFunc(out, func(m []byte) []byte {
		return []byte(s.fakeEmail(string(m)))
	})
	for _, r := range s.redact {
		out = bytes.ReplaceAll(out, []byte(r[0]), []byte(r[1]))
	}
	return out
}

// scrubAvatarFields replaces every string under a key beginning "avatar"
// (avatar_url, avatars_sample, …) with the placeholder, on the decoded
// document — so escaped key spellings and arrays of URLs are covered, which
// the byte-level pattern cannot see. A body with nothing to replace, or
// that is not JSON, is returned unchanged.
func scrubAvatarFields(body []byte) []byte {
	if !json.Valid(body) {
		return body // a non-JSON body (JSON with a trailer, say) stays whole
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		return body
	}
	changed := false
	var walk func(v any, under bool) any
	walk = func(v any, under bool) any {
		switch t := v.(type) {
		case string:
			if under && t != "" && t != "https://example.com/avatar.png" {
				changed = true
				return "https://example.com/avatar.png"
			}
			return t
		case map[string]any:
			for k, c := range t {
				t[k] = walk(c, under || strings.HasPrefix(k, "avatar")) // nested under an avatar key stays under it
			}
			return t
		case []any:
			for i, c := range t {
				t[i] = walk(c, under)
			}
			return t
		}
		return v
	}
	doc = walk(doc, false)
	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(doc) != nil {
		return body
	}
	return bytes.TrimSpace(buf.Bytes())
}

// fakeEmail maps a real address to a stable placeholder: an HMAC of the
// lowercased address under the scrubber key.
func (s *Scrubber) fakeEmail(addr string) string {
	if strings.HasSuffix(strings.ToLower(addr), "@example.com") {
		return addr // already a fixture (or already scrubbed)
	}
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(strings.ToLower(addr)))
	return fmt.Sprintf("person-%x@example.com", m.Sum(nil)[:5])
}
