package exchange

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/basecamp/mcp/auth/upstream"
)

// ParseOAuthError is the single boundary both the HTTP-mode token exchange
// (this package) and a product's stdio login path funnel an
// endpoint-controlled OAuth error body through, so the two cannot drift on
// what a malicious or broken authorization server is allowed to place in a
// log, transcript, or error string. The body MUST already be length-bounded
// (see MaxResponseBytes) — this reads it, it does not cap it.
//
// It returns the allowlisted RFC 6749 §5.2 error code together with an
// error_description from which every credential the request submitted has
// been redacted. code is "" only when the body carries no `error` member
// (unparseable or non-OAuth), which the caller treats as an endpoint
// malfunction rather than a classified rejection; a recognized-but-
// unregistered code collapses to "unrecognized_error" (see knownOAuthCodes)
// and is never "". Fields other than `error` and `error_description` are
// never read, so a credential an endpoint hides in some unexpected member
// never reaches the returned strings at all.
func ParseOAuthError(body []byte, form url.Values) (code, description string) {
	var oe struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &oe); err != nil || oe.Error == "" {
		return "", ""
	}
	// The `error` member is endpoint-controlled text just like the
	// description. OAuth error codes are a closed vocabulary, so the code is
	// checked ONCE here against the enumerated allowlist (knownOAuthCodes —
	// deliberately NOT a shape check, which would admit token-shaped values):
	// anything unlisted, token echoes included, collapses to a placeholder
	// before it can reach any sink.
	code = sanitizeOAuthCode(oe.Error)
	// Control characters are stripped BEFORE redaction: a credential split by
	// a newline or DEL would dodge ReplaceAll and then reassemble in a later
	// stripping pass.
	description = redactCredentials(stripControls(oe.ErrorDescription), form)
	return code, description
}

// stripControls removes the runes upstream.LogSafeRune rejects (controls,
// line and paragraph separators, format characters, U+FFFD). Applied to both the
// endpoint text and each redaction candidate so a secret carrying a tab
// arrives control-stripped in the echoed text and only an identically
// normalized candidate can match it.
func stripControls(v string) string {
	var b strings.Builder
	for _, r := range v {
		// The same runes upstream.ScrubDescription drops when rendering:
		// one left in here would split a credential past redaction and
		// rejoin it in the rendered text.
		if upstream.LogSafeRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// credentialFields are the form parameters whose values are secrets at ANY
// length, spanning both the token-exchange form (subject_token,
// client_assertion) and the stdio login forms (code, code_verifier,
// client_secret, refresh_token). A value in any of these is redacted even
// when short (a six-byte secret is still a secret); every other field is
// redacted only as a >= 8-byte backstop.
var credentialFields = map[string]bool{
	"subject_token":    true,
	"client_assertion": true,
	"client_secret":    true,
	"code":             true,
	"code_verifier":    true,
	"refresh_token":    true,
}

// redactCredentials replaces every credential the request submitted with
// [redacted] in desc, which must already be control-stripped. Values are
// replaced LONGEST FIRST so a shorter value that prefixes a credential
// (client_id inside client_secret) cannot be replaced first and leave the
// secret's suffix behind. The form-encoded spelling (what the wire carried)
// is a candidate too. A candidate that is empty after control-stripping is
// dropped, so an all-control value never turns into a match-everything "".
// A credential field's value is then redacted piecewise too (see
// redactFragments), so a partial echo of it is caught as well.
func redactCredentials(desc string, form url.Values) string {
	var redact, fragments []string
	for field, vs := range form {
		credentialField := credentialFields[field]
		for _, v := range vs {
			if v == "" || (!credentialField && len(v) < 8) {
				continue
			}
			var spellings []string
			if s := stripControls(v); s != "" {
				spellings = append(spellings, s)
			}
			if enc := url.QueryEscape(v); enc != v {
				if s := stripControls(enc); s != "" {
					spellings = append(spellings, s)
				}
			}
			redact = append(redact, spellings...)
			if credentialField {
				fragments = append(fragments, spellings...)
			}
		}
	}
	sort.Slice(redact, func(i, j int) bool { return len(redact[i]) > len(redact[j]) })
	for _, v := range redact {
		desc = strings.ReplaceAll(desc, v, "[redacted]")
	}
	return redactFragments(desc, fragments)
}

// fragmentBytes is the shortest piece of a credential redactFragments
// recognises. Any echoed piece at least this long is made only of such
// pieces, so it is covered whole: a truncated, prefixed or partial echo is
// redacted as surely as the full value.
const fragmentBytes = 8

// redactFragments replaces with [redacted] every run of desc made of
// fragmentBytes-long pieces of any credential in creds. The full-value pass
// can't see an endpoint that echoes only part of a credential (a prefix cut
// at a length limit, say); this one does, whatever the cut.
func redactFragments(desc string, creds []string) string {
	pieces := map[string]bool{}
	for _, c := range creds {
		for i := 0; i+fragmentBytes <= len(c); i++ {
			pieces[c[i:i+fragmentBytes]] = true
		}
	}
	if len(pieces) == 0 || len(desc) < fragmentBytes {
		return desc
	}
	covered := make([]bool, len(desc))
	for i := 0; i+fragmentBytes <= len(desc); i++ {
		if pieces[desc[i:i+fragmentBytes]] {
			for j := i; j < i+fragmentBytes; j++ {
				covered[j] = true
			}
		}
	}
	var b strings.Builder
	for i := 0; i < len(desc); {
		if !covered[i] {
			b.WriteByte(desc[i])
			i++
			continue
		}
		for i < len(desc) && covered[i] {
			i++
		}
		b.WriteString("[redacted]")
	}
	return b.String()
}
