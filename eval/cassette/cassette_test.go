package cassette

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func do(t *testing.T, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestPlayerMatchesAndLogs(t *testing.T) {
	base := &Cassette{Name: "base", Interactions: []Interaction{
		{Request: Request{Method: "GET", Path: "/1/projects.json"},
			Response: Response{Status: 200, Body: json.RawMessage(`[{"id":1,"url":"{{base}}/1/projects/1.json"}]`),
				Headers: map[string]string{"Link": `<{{base}}/1/projects.json?page=2>; rel="next"`}}},
		{Request: Request{Method: "GET", Path: "/1/projects.json", Query: map[string]string{"page": "2"}},
			Response: Response{Status: 200, Body: json.RawMessage(`[{"id":2}]`)}},
		{Request: Request{Method: "GET", Path: "/1/todos/5"}, Response: Response{Status: 200, Body: json.RawMessage(`{"v":"base"}`)}},
		{Request: Request{Method: "POST", Path: "/1/todos/5/completion.json"}, Response: Response{Status: 204}},
	}}
	task := &Cassette{Name: "task", Interactions: []Interaction{
		{Request: Request{Method: "GET", Path: "/1/todos/5"}, Response: Response{Status: 200, Body: json.RawMessage(`{"v":"task-1"}`)}},
		{Request: Request{Method: "GET", Path: "/1/todos/5"}, Response: Response{Status: 200, Body: json.RawMessage(`{"v":"task-2"}`)}},
	}}
	p := NewPlayer(base, task)
	url := p.Start()
	defer p.Close()

	// {{base}} is substituted in bodies and headers; unrelated query params
	// are ignored (subset match).
	status, body, hdr := get(t, url+"/1/projects.json?status=active")
	assert.Equal(t, 200, status)
	assert.Contains(t, body, url+"/1/projects/1.json")
	assert.Contains(t, hdr.Get("Link"), url+"/1/projects.json?page=2")

	// The most specific query match wins.
	_, body, _ = get(t, url+"/1/projects.json?page=2")
	assert.JSONEq(t, `[{"id":2}]`, body)

	// A later cassette overrides an earlier one; identical patterns in one
	// cassette are served in order, the last repeating. ".json" is optional.
	_, body, _ = get(t, url+"/1/todos/5.json")
	assert.JSONEq(t, `{"v":"task-1"}`, body)
	_, body, _ = get(t, url+"/1/todos/5")
	assert.JSONEq(t, `{"v":"task-2"}`, body)
	_, body, _ = get(t, url+"/1/todos/5")
	assert.JSONEq(t, `{"v":"task-2"}`, body)

	// Writes are answered by path and logged with the body sent.
	assert.Equal(t, 204, do(t, "POST", url+"/1/todos/5/completion.json", `{ "a" : 1 }`))

	// A miss is a 404, logged as unmatched.
	status, _, _ = get(t, url+"/1/todos/999")
	assert.Equal(t, 404, status)

	log := p.Log()
	require.Len(t, log, 7)
	assert.Equal(t, "status=active", log[0].Query)
	assert.True(t, log[5].IsWrite())
	assert.Equal(t, `POST /1/todos/5/completion.json {"a":1}`, log[5].Line())
	assert.False(t, log[6].Matched)
	assert.True(t, log[0].Matched)

	mark := 5
	assert.Len(t, p.Since(mark), 2)
	assert.Equal(t, 7, p.Len())
}

func TestLoadValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(s), 0o644))
		return path
	}
	for name, body := range map[string]string{
		"noname.json":   `{"interactions":[]}`,
		"method.json":   `{"name":"x","interactions":[{"request":{"method":"TRACE","path":"/a"},"response":{"status":200}}]}`,
		"query.json":    `{"name":"x","interactions":[{"request":{"method":"GET","path":"/a?b=1"},"response":{"status":200}}]}`,
		"status.json":   `{"name":"x","interactions":[{"request":{"method":"GET","path":"/a"},"response":{"status":0}}]}`,
		"unknown.json":  `{"name":"x","interactions":[],"extra":1}`,
		"bothbody.json": `{"name":"x","interactions":[{"request":{"method":"GET","path":"/a"},"response":{"status":200,"body":{},"body_text":"x"}}]}`,
	} {
		_, err := Load(write(name, body))
		assert.Error(t, err, name)
	}
	c, err := Load(write("ok.json", `{"name":"ok","interactions":[{"request":{"method":"GET","path":"/a"},"response":{"status":200,"body":{"a":1}}}]}`))
	require.NoError(t, err)
	assert.Equal(t, "ok", c.Name)

	// Round trip.
	out := filepath.Join(dir, "saved.json")
	require.NoError(t, c.Save(out))
	back, err := Load(out)
	require.NoError(t, err)
	assert.Equal(t, c.Interactions[0].Request, back.Interactions[0].Request)
}

func TestProfileValidation(t *testing.T) {
	good := Profile{Name: "seed", TestAccount: true, Upstream: "https://3.basecampapi.com", AccountIDs: []string{"123"}, TokenEnv: "EVAL_TOKEN"}
	require.NoError(t, good.Validate())

	cases := map[string]func(p *Profile){
		"not declared test":  func(p *Profile) { p.TestAccount = false },
		"cleartext upstream": func(p *Profile) { p.Upstream = "http://3.basecampapi.com" },
		"upstream with path": func(p *Profile) { p.Upstream = "https://3.basecampapi.com/123" },
		"no accounts":        func(p *Profile) { p.AccountIDs = nil },
		"non-numeric acct":   func(p *Profile) { p.AccountIDs = []string{"abc"} },
		"no token env":       func(p *Profile) { p.TokenEnv = "" },
		"quote in redact":    func(p *Profile) { p.Redact = map[string]string{`A "B"`: "C"} },
	}
	for name, mutate := range cases {
		p := good
		mutate(&p)
		assert.Error(t, p.Validate(), name)
	}
}

func TestRecorderProxiesScrubsAndRefusesOtherAccounts(t *testing.T) {
	var gotAuth []string
	var upstreamHits []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		upstreamHits = append(upstreamHits, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=secret")
		w.Header().Set("X-Request-Id", "abc")
		self := "https://" + r.Host
		switch r.URL.Path {
		case "/123/people.json":
			_, _ = io.WriteString(w, `[{"id":1,"name":"Real Person","email_address":"real.person@corp.example","avatar_url":"https://cdn.example/avatar?sig=SECRET","url":"`+self+`/123/people/1.json"}]`)
		default:
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer upstream.Close()

	t.Setenv("EVAL_REC_TOKEN", "real-token")
	p := &Profile{Name: "seed", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"123"}, TokenEnv: "EVAL_REC_TOKEN",
		Redact: map[string]string{"Real Person": "Fixture Person"}}
	rec, err := NewRecorder(p)
	require.NoError(t, err)
	rec.client = upstream.Client()
	url := rec.Start()
	defer rec.Close()

	req, _ := http.NewRequest("GET", url+"/123/people.json", nil)
	req.Header.Set("Authorization", "Bearer dummy")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	live, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// The live answer points follow-ups back at the recorder.
	assert.Contains(t, string(live), url+"/123/people/1.json")

	assert.Equal(t, 201, do(t, "POST", url+"/123/comments.json", `{"content":"hi real.person@corp.example"}`))

	// Another account is refused locally and never reaches the upstream.
	assert.Equal(t, 403, do(t, "GET", url+"/999/people.json", ""))
	assert.Len(t, upstreamHits, 2)
	assert.Equal(t, []string{"Bearer real-token", "Bearer real-token"}, gotAuth, "the profile token replaces the server's dummy")

	c := rec.Cassette("people", "")
	data, err := json.Marshal(c)
	require.NoError(t, err)
	s := string(data)
	for _, leaked := range []string{"real-token", "dummy", "session=secret", "X-Request-Id", "real.person@corp.example", "SECRET", "Real Person", upstream.URL} {
		assert.NotContains(t, s, leaked)
	}
	assert.Contains(t, s, BasePlaceholder+"/123/people/1.json")
	alias := regexp.MustCompile(`person-[0-9a-f]{10}@example\.com`).FindString(s)
	require.NotEmpty(t, alias)
	assert.Contains(t, s, "Fixture Person")
	require.Len(t, c.Interactions, 2)
	assert.JSONEq(t, `{"content":"hi `+alias+`"}`, string(c.Interactions[1].Request.Body))
	// The live answer the server saw was scrubbed too.
	assert.NotContains(t, string(live), "real.person@corp.example")
	assert.Contains(t, string(live), alias)

	// The recorder keeps the same exchange log a player does.
	log := rec.Since(0)
	require.Len(t, log, 3)
	assert.Equal(t, 403, log[2].Status)

	// A recorded cassette replays.
	pl := NewPlayer(c)
	purl := pl.Start()
	defer pl.Close()
	status, body, _ := get(t, purl+"/123/people.json")
	assert.Equal(t, 200, status)
	assert.Contains(t, body, purl+"/123/people/1.json")
}

func TestNewRecorderNeedsToken(t *testing.T) {
	t.Setenv("EVAL_MISSING_TOKEN", "")
	_, err := NewRecorder(&Profile{Name: "seed", TestAccount: true, Upstream: "https://x.example", AccountIDs: []string{"1"}, TokenEnv: "EVAL_MISSING_TOKEN"})
	assert.ErrorContains(t, err, "EVAL_MISSING_TOKEN is not set")
}

func TestRecorderRedirectsQueriesAndRepeatedReads(t *testing.T) {
	state := "before"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/123/moved":
			http.Redirect(w, r, "/999/secret.json", http.StatusFound)
		case r.URL.Path == "/999/secret.json":
			t.Error("the upstream was asked for an account outside the profile")
		case r.Method == "POST":
			state = "after"
			w.WriteHeader(204)
		default:
			_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
		}
	}))
	defer upstream.Close()
	t.Setenv("EVAL_REC_TOKEN", "tok")
	rec, err := NewRecorder(&Profile{Name: "seed", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"123"}, TokenEnv: "EVAL_REC_TOKEN"})
	require.NoError(t, err)
	tr := upstream.Client().Transport
	rec.client.Transport = tr
	url := rec.Start()
	defer rec.Close()

	// The redirect is handed back, not followed; following it through the
	// recorder is refused.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(url + "/123/moved")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, 403, do(t, "GET", url+"/999/secret.json", ""))

	// Query values are scrubbed in the cassette and the log.
	get(t, url+"/123/search.json?q=someone@corp.example")
	// read, read (unchanged: dropped), write, read (changed: kept).
	get(t, url+"/123/thing.json")
	get(t, url+"/123/thing.json")
	do(t, "POST", url+"/123/thing/touch.json", "")
	get(t, url+"/123/thing.json")

	c := rec.Cassette("x", "")
	data, _ := json.Marshal(c)
	assert.NotContains(t, string(data), "someone@corp.example")
	for _, ex := range rec.Since(0) {
		assert.NotContains(t, ex.Query, "someone@corp.example")
	}
	var reads []string
	for _, in := range c.Interactions {
		if in.Request.Path == "/123/thing.json" {
			reads = append(reads, string(in.Response.Body))
		}
	}
	assert.Equal(t, []string{`{"state":"before"}`, `{"state":"after"}`}, reads)

	// Replay walks the same states.
	p := NewPlayer(c)
	purl := p.Start()
	defer p.Close()
	_, b1, _ := get(t, purl+"/123/thing.json")
	_, b2, _ := get(t, purl+"/123/thing.json")
	assert.JSONEq(t, `{"state":"before"}`, b1)
	assert.JSONEq(t, `{"state":"after"}`, b2)
}

func TestScrubberAliasesAreKeyedAndStable(t *testing.T) {
	a := NewScrubber("", nil, "k1").String("x@corp.example")
	assert.Equal(t, a, NewScrubber("", nil, "k1").String("X@corp.example"))
	assert.NotEqual(t, a, NewScrubber("", nil, "k2").String("x@corp.example"))
	assert.Equal(t, "keep@example.com", NewScrubber("", nil, "k1").String("keep@example.com"))
}

func TestMergeKeepsFirstAnswerPerPattern(t *testing.T) {
	a := &Cassette{Name: "a", Interactions: []Interaction{{Request: Request{Method: "GET", Path: "/x"}, Response: Response{Status: 200, Body: json.RawMessage(`1`)}}}}
	b := &Cassette{Name: "b", Interactions: []Interaction{
		{Request: Request{Method: "GET", Path: "/x.json"}, Response: Response{Status: 200, Body: json.RawMessage(`2`)}},
		{Request: Request{Method: "GET", Path: "/y"}, Response: Response{Status: 200, Body: json.RawMessage(`3`)}},
	}}
	b.Interactions = append(b.Interactions, Interaction{Request: Request{Method: "GET", Path: "/y"}, Response: Response{Status: 200, Body: json.RawMessage(`4`)}})
	Merge(a, b)
	require.Len(t, a.Interactions, 3, "every recorded state of a new pattern")
	assert.Equal(t, json.RawMessage(`1`), a.Interactions[0].Response.Body)
	assert.Equal(t, "/y", a.Interactions[1].Request.Path)
}
