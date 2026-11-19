// Package confirm is the person's confirmation: a one-field form elicitation
// a host shows the person before a tool does what a model's word alone
// shouldn't (send mail, let a sender through, delete for good), and the
// HMAC-signed state that carries the question between the rounds.
//
// On MCP 2026-07-28 the question travels as an input_required result
// (SEP-2322) carrying the signed state; the client retries the call with
// the state and the person's answer. On a legacy stdio session the SDK turns
// the same result into a server-initiated elicitation/create. Stateless
// legacy HTTP can't ask at all (CanAsk).
//
// The state binds the tool, its arguments, an identity and a grant, and a
// digest of what the person was shown (State), so an answer can't be
// replayed against a different call, person, connection or target. The
// caller recomputes the digest at the retry, before acting, so a change made
// after the person approved fails the check.
//
// What a tool asks, in what order it checks, and what a "no" returns stay
// in each product. Extracted from hey-mcp-server's confirm.go and hey-cli's
// send_draft, which each carried a copy.
package confirm

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TTL bounds how long a question stays answerable: a confirmation shouldn't
// outlive the person's attention.
const TTL = 5 * time.Minute

// MinKeyBytes is the shortest HMAC key accepted.
const MinKeyBytes = 32

// Why a state didn't verify. Each is a reason to do nothing.
var (
	ErrMalformed  = errors.New("malformed confirmation state")
	ErrUnknownKey = errors.New("confirmation state signed with an unknown key")
	ErrSignature  = errors.New("confirmation state signature mismatch")
	ErrExpired    = errors.New("the confirmation expired; nothing was done")
)

// Keys sign and check confirmation state. Verification accepts any listed
// key and signing uses the active one, so keys rotate without failing
// questions already asked.
type Keys struct {
	keys   map[string][]byte
	active string
}

// ParseKeys parses "id1:hex,id2:hex" and the active key's id. Both empty is
// (nil, nil): nothing configured.
func ParseKeys(keysCSV, active string) (*Keys, error) {
	keysCSV, active = strings.TrimSpace(keysCSV), strings.TrimSpace(active)
	if keysCSV == "" && active == "" {
		return nil, nil
	}
	if keysCSV == "" || active == "" {
		return nil, errors.New("confirmation keys need both the keys and the active key's id")
	}
	keys := map[string][]byte{}
	for _, entry := range strings.Split(keysCSV, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, hexKey, ok := strings.Cut(entry, ":")
		if !ok || id == "" {
			return nil, errors.New("each confirmation key is id:hex")
		}
		key, err := hex.DecodeString(hexKey)
		if err != nil {
			return nil, fmt.Errorf("confirmation key %q isn't hex", id)
		}
		if len(key) < MinKeyBytes {
			return nil, fmt.Errorf("confirmation key %q is shorter than %d bytes", id, MinKeyBytes)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("confirmation key %q is listed twice", id)
		}
		keys[id] = key
	}
	if _, ok := keys[active]; !ok {
		return nil, fmt.Errorf("the active confirmation key %q isn't among the keys", active)
	}
	return &Keys{keys: keys, active: active}, nil
}

// EphemeralKeys is one random key for this process alone. A question it
// signs is answerable only here: right for stdio and a single replica, and
// fail-closed across replicas (the answer is refused, nothing acts).
func EphemeralKeys() (*Keys, error) {
	key := make([]byte, MinKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &Keys{keys: map[string][]byte{"ephemeral": key}, active: "ephemeral"}, nil
}

// State is the signed payload of one question.
type State struct {
	// KeyID is the key that signed it; Sign sets it.
	KeyID string `json:"kid"`
	// Expires is the Unix second after which the answer is refused.
	Expires int64 `json:"exp"`
	// Kind and Method mark the state as this product's confirmation, so no
	// other signed-state validator accepts it: name a method no real
	// request carries. Move the kind's version whenever a field joins the
	// binding, so a build that predates the field refuses the state instead
	// of ignoring it.
	Kind   string `json:"kind"`
	Method string `json:"method"`
	// Tool and ArgsHash (HashArgs) are the call the question was asked of.
	Tool     string `json:"tool"`
	ArgsHash string `json:"args"`
	// Identity is who was asked.
	Identity string `json:"identity"`
	// Grant is the OAuth grant (or sign-in) the question was asked under,
	// so an answer given through one connection can't be redeemed through
	// another of the same person's. Empty where there is no grant to name.
	Grant string `json:"grant,omitempty"`
	// Digest (Digest) fingerprints what the person was shown. The caller
	// recomputes and compares it at the retry (Matches leaves it out), so
	// it can say "changed since you approved it" apart from "doesn't match".
	Digest string `json:"digest"`
}

// Matches reports whether a verified state was asked of the call described
// by want: the same kind, method, tool, arguments, identity and grant. The
// key, expiry and digest aren't compared.
func (s State) Matches(want State) bool {
	return s.Kind == want.Kind && s.Method == want.Method &&
		s.Tool == want.Tool && s.ArgsHash == want.ArgsHash &&
		s.Identity == want.Identity && s.Grant == want.Grant
}

// Sign signs a state with the active key, setting its KeyID:
// base64url(json) "." base64url(HMAC-SHA256).
func (k *Keys) Sign(state State) (string, error) {
	state.KeyID = k.active
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, k.keys[k.active])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify checks a state's signature against the keyring and its expiry at
// now. The caller then checks what it was asked of (Matches) and its digest.
func (k *Keys) Verify(blob string, now time.Time) (State, error) {
	var zero State
	payloadB64, sigB64, ok := strings.Cut(blob, ".")
	if !ok {
		return zero, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return zero, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return zero, ErrMalformed
	}
	var state State
	if err := json.Unmarshal(payload, &state); err != nil {
		return zero, ErrMalformed
	}
	key, ok := k.keys[state.KeyID]
	if !ok {
		return zero, ErrUnknownKey
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return zero, ErrSignature
	}
	if now.Unix() > state.Expires {
		return zero, ErrExpired
	}
	return state, nil
}

// HashArgs hashes a call's arguments canonically (encoding/json sorts map
// keys).
func HashArgs(args any) string {
	data, err := json.Marshal(args)
	if err != nil {
		return "unhashable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Digest hashes what a person is asked to approve.
func Digest(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "undigestable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
