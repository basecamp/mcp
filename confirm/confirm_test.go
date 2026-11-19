package confirm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var hexKey = strings.Repeat("ab", 32)

func asked() State {
	return State{
		Kind: "confirm.v2", Method: "tools/call\x00confirm",
		Tool: "send_draft", ArgsHash: HashArgs(map[string]any{"draft_id": 9}),
		Identity: "1", Grant: "g1", Digest: Digest(map[string]any{"subject": "hi"}),
	}
}

func TestParseKeys(t *testing.T) {
	keys, err := ParseKeys("", "")
	assert.NoError(t, err)
	assert.Nil(t, keys)
	keys, err = ParseKeys("k1:"+hexKey+", k2:"+strings.Repeat("cd", 32), "k2")
	require.NoError(t, err)
	assert.Equal(t, "k2", keys.active)
	for _, bad := range [][2]string{{"k1:" + hexKey, ""}, {"", "k1"}, {"k1:zz", "k1"}, {"k1:abcd", "k1"}, {"k1:" + hexKey, "k9"}, {"k1:" + hexKey + ",k1:" + hexKey, "k1"}, {":" + hexKey, ""}, {":" + hexKey, "k1"}, {"k1" + hexKey, "k1"}} {
		_, err := ParseKeys(bad[0], bad[1])
		assert.Error(t, err, bad)
	}

	// A state signed with one key verifies against a ring that lists it.
	old, err := ParseKeys("k1:"+hexKey, "k1")
	require.NoError(t, err)
	state := asked()
	state.Expires = time.Now().Add(time.Minute).Unix()
	blob, err := old.Sign(state)
	require.NoError(t, err)
	got, err := keys.Verify(blob, time.Now())
	require.NoError(t, err, "rotation keeps open questions answerable")
	assert.Equal(t, "k1", got.KeyID)
	assert.True(t, got.Matches(asked()))
	assert.Equal(t, state.Digest, got.Digest)

	// Signing uses the active key.
	blob, err = keys.Sign(state)
	require.NoError(t, err)
	_, err = old.Verify(blob, time.Now())
	assert.ErrorIs(t, err, ErrUnknownKey)
}

func TestEphemeralKeysAreThisProcesssAlone(t *testing.T) {
	one, err := EphemeralKeys()
	require.NoError(t, err)
	other, err := EphemeralKeys()
	require.NoError(t, err)
	state := asked()
	state.Expires = time.Now().Add(TTL).Unix()
	blob, err := one.Sign(state)
	require.NoError(t, err)
	_, err = one.Verify(blob, time.Now())
	assert.NoError(t, err)
	_, err = other.Verify(blob, time.Now())
	assert.ErrorIs(t, err, ErrSignature, "same key id, another key")
}

// A state is answerable through its expiry second and refused after it.
func TestStateExpires(t *testing.T) {
	keys, err := ParseKeys("k1:"+hexKey, "k1")
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	state := asked()
	state.Expires = now.Add(TTL).Unix()
	blob, err := keys.Sign(state)
	require.NoError(t, err)
	_, err = keys.Verify(blob, now.Add(TTL))
	assert.NoError(t, err)
	_, err = keys.Verify(blob, now.Add(TTL+time.Second))
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyRefusesWhatItDidntSign(t *testing.T) {
	keys, err := ParseKeys("k1:"+hexKey, "k1")
	require.NoError(t, err)
	state := asked()
	state.Expires = time.Now().Add(TTL).Unix()
	blob, err := keys.Sign(state)
	require.NoError(t, err)
	payload, sig, _ := strings.Cut(blob, ".")

	raw, err := base64.RawURLEncoding.DecodeString(payload)
	require.NoError(t, err)
	var forged map[string]any
	require.NoError(t, json.Unmarshal(raw, &forged))
	forged["tool"] = "delete_todo"
	edited, err := json.Marshal(forged)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		blob string
		want error
	}{
		"no separator":      {payload, ErrMalformed},
		"payload not b64":   {"!!." + sig, ErrMalformed},
		"signature not b64": {payload + ".!!", ErrMalformed},
		"payload not json":  {base64.RawURLEncoding.EncodeToString([]byte("nope")) + "." + sig, ErrMalformed},
		"edited payload":    {base64.RawURLEncoding.EncodeToString(edited) + "." + sig, ErrSignature},
		"empty":             {"", ErrMalformed},
	} {
		_, err := keys.Verify(tc.blob, time.Now())
		assert.ErrorIs(t, err, tc.want, name)
	}
}

// A verified state matches only the call it was asked of: another tool,
// other arguments, another identity or grant, or another product's kind or
// method is refused. The digest is the caller's to compare.
func TestStateIsBoundToTheCall(t *testing.T) {
	want := asked()
	assert.True(t, want.Matches(want))
	changed := want
	changed.Digest, changed.Expires, changed.KeyID = "other", 1, "k9"
	assert.True(t, changed.Matches(want), "digest, expiry and key aren't the binding")
	for name, edit := range map[string]func(*State){
		"kind":     func(s *State) { s.Kind = "confirm" },
		"method":   func(s *State) { s.Method = "tools/call\x00send_draft" },
		"tool":     func(s *State) { s.Tool = "delete_todo" },
		"args":     func(s *State) { s.ArgsHash = HashArgs(map[string]any{"draft_id": 10}) },
		"identity": func(s *State) { s.Identity = "2" },
		"grant":    func(s *State) { s.Grant = "" },
	} {
		other := want
		edit(&other)
		assert.False(t, other.Matches(want), name)
	}
}

func TestHashesAreCanonical(t *testing.T) {
	assert.Equal(t, HashArgs(map[string]any{"a": 1, "b": 2}), HashArgs(map[string]any{"b": 2, "a": 1}))
	assert.NotEqual(t, HashArgs(map[string]any{"a": 1}), HashArgs(map[string]any{"a": 2}))
	assert.Equal(t, "unhashable", HashArgs(func() {}))
	assert.Equal(t, "undigestable", Digest(make(chan int)))
	assert.Len(t, Digest("x"), 64)
}

// A claim is forgotten at its TTL whether or not another claim comes to
// sweep it, and forgetting an older claim leaves a newer one of the same id.
func TestClaimsForgetAtTheTTL(t *testing.T) {
	claims := NewClaims(24 * time.Hour)
	then := time.Now()
	require.True(t, claims.Claim("9", then))
	assert.True(t, claims.Claimed("9", then.Add(time.Minute)))
	require.False(t, claims.Claim("9", then.Add(time.Minute)), "taken within the TTL")
	claims.forget("9", then.Add(-time.Hour))
	assert.True(t, claims.Claimed("9", then), "a stale timer leaves the claim it didn't take")
	claims.forget("9", then)
	assert.False(t, claims.Claimed("9", then), "its own timer drops it")
	assert.Empty(t, claims.claims)
}

func TestClaimsSweepOnClaim(t *testing.T) {
	claims := NewClaims(time.Hour)
	then := time.Now()
	require.True(t, claims.Claim("1", then))
	assert.False(t, claims.Claimed("1", then.Add(time.Hour)), "expired at the TTL")
	require.True(t, claims.Claim("2", then.Add(time.Hour)))
	assert.NotContains(t, claims.claims, "1", "swept by the next claim")
	require.True(t, claims.Claim("1", then.Add(time.Hour)), "claimable again")
}

func TestClaimsTimerForgets(t *testing.T) {
	claims := NewClaims(10 * time.Millisecond)
	require.True(t, claims.Claim("9", time.Now()))
	assert.Eventually(t, func() bool {
		claims.mu.Lock()
		defer claims.mu.Unlock()
		return len(claims.claims) == 0
	}, time.Second, 5*time.Millisecond)
}

type client struct {
	protocol string
	caps     *mcp.ClientCapabilities
}

func (c client) ProtocolVersion() string                     { return c.protocol }
func (c client) ClientCapabilities() *mcp.ClientCapabilities { return c.caps }

func TestCanAsk(t *testing.T) {
	form := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}
	empty := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{}}
	urlOnly := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}}
	none := &mcp.ClientCapabilities{}
	for name, tc := range map[string]struct {
		req   ClientRequest
		t     Transport
		could bool
	}{
		"nil":                   {nil, TransportStdio, false},
		"legacy stdio form":     {client{"2025-11-25", form}, TransportStdio, true},
		"legacy stdio empty":    {client{"2025-11-25", empty}, TransportStdio, true},
		"legacy stdio url only": {client{"2025-11-25", urlOnly}, TransportStdio, false},
		"legacy stdio none":     {client{"2025-11-25", none}, TransportStdio, false},
		"legacy stdio nil caps": {client{"2025-11-25", nil}, TransportStdio, false},
		"legacy http form":      {client{"2025-11-25", form}, TransportHTTP, false},
		"modern http form":      {client{ProtocolMRTR, form}, TransportHTTP, true},
		"modern http empty":     {client{ProtocolMRTR, empty}, TransportHTTP, true},
		"modern http url only":  {client{ProtocolMRTR, urlOnly}, TransportHTTP, false},
		"modern http none":      {client{ProtocolMRTR, none}, TransportHTTP, false},
	} {
		assert.Equal(t, tc.could, CanAsk(tc.req, tc.t), name)
	}
}

func TestElicitationAsksOneRequiredBoolean(t *testing.T) {
	p := Elicitation("Send it?", Field{Title: "Confirm", Description: "Yes goes ahead."})
	assert.Equal(t, "form", p.Mode)
	assert.Equal(t, "Send it?", p.Message)
	data, err := json.Marshal(p.RequestedSchema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object","properties":{"confirm":{"type":"boolean","title":"Confirm","description":"Yes goes ahead."}},"required":["confirm"]}`, string(data))
}

func TestAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		responses mcp.InputResponseMap
		yes, err  bool
	}{
		"accept yes":   {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}, true, false},
		"accept no":    {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": false}}}, false, false},
		"decline":      {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "decline"}}, false, false},
		"cancel":       {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "cancel"}}, false, false},
		"missing":      {mcp.InputResponseMap{}, false, true},
		"other key":    {mcp.InputResponseMap{"confirm_send": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}, false, true},
		"nil":          {mcp.InputResponseMap{"confirm": nil}, false, true},
		"typed nil":    {mcp.InputResponseMap{"confirm": (*mcp.ElicitResult)(nil)}, false, true},
		"odd action":   {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "maybe", Content: map[string]any{"confirm": true}}}, false, true},
		"string yes":   {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": "true"}}}, false, true},
		"no content":   {mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept"}}, false, true},
		"not a result": {mcp.InputResponseMap{"confirm": &mcp.CreateMessageResult{}}, false, true},
	} {
		yes, err := Answer(tc.responses, "confirm")
		assert.Equal(t, tc.yes, yes, name)
		assert.Equal(t, tc.err, err != nil, name)
	}
}

func TestFilterToolsCopiesTheList(t *testing.T) {
	tools := []*mcp.Tool{{Name: "read"}, {Name: "send"}}
	served := &mcp.ListToolsResult{Tools: tools, NextCursor: "c"}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return served, nil }
	var asked *mcp.ListToolsRequest
	mw := FilterTools(func(req *mcp.ListToolsRequest) func(string) bool {
		asked = req
		return func(name string) bool { return name != "send" }
	})
	req := &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}}
	res, err := mw(next)(context.Background(), "tools/list", req)
	require.NoError(t, err)
	list := res.(*mcp.ListToolsResult)
	assert.Equal(t, []string{"read"}, names(list.Tools))
	assert.Equal(t, "c", list.NextCursor)
	assert.Same(t, req, asked)
	assert.Equal(t, []string{"read", "send"}, names(served.Tools), "the served list is untouched")

	call := &mcp.CallToolResult{}
	res, err = FilterTools(func(*mcp.ListToolsRequest) func(string) bool { panic("not a list") })(
		func(context.Context, string, mcp.Request) (mcp.Result, error) { return call, nil })(context.Background(), "tools/call", &mcp.CallToolRequest{})
	require.NoError(t, err)
	assert.Same(t, call, res, "other methods pass through")
}

func names(tools []*mcp.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
