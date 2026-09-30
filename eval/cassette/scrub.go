package cassette

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Scrubber removes what a cassette must not carry: the live API origin
// (replaced with BasePlaceholder), email addresses (replaced with stable
// per-address fakes, so one person stays one person across a cassette),
// avatar URLs (which embed signed CDN tokens), and caller-listed literals.
// It works on raw text as well as JSON so request bodies, response bodies and
// header values are all covered by the same rules.
type Scrubber struct {
	upstream string
	redact   [][2]string // longest first, so a full name replaces before a first name

	mu     sync.Mutex
	emails map[string]string
}

var (
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	avatarRE = regexp.MustCompile(`"(avatar_url|avatar_url_large|avatar_thumbnail_url)"\s*:\s*"[^"]*"`)
)

// NewScrubber builds a scrubber for an upstream origin and literal redactions.
func NewScrubber(upstream string, redact map[string]string) *Scrubber {
	s := &Scrubber{upstream: strings.TrimSuffix(upstream, "/"), emails: map[string]string{}}
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
		out = bytes.ReplaceAll(out, []byte(s.upstream), []byte(BasePlaceholder))
		// JSON encoders may escape the slashes in an absolute URL.
		out = bytes.ReplaceAll(out, []byte(strings.ReplaceAll(s.upstream, "/", `\/`)), []byte(BasePlaceholder))
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

// fakeEmail maps a real address to a stable placeholder.
func (s *Scrubber) fakeEmail(addr string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(addr)
	if f, ok := s.emails[key]; ok {
		return f
	}
	f := fmt.Sprintf("person%d@example.com", len(s.emails)+1)
	s.emails[key] = f
	return f
}
