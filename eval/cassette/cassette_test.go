package cassette

import (
	"encoding/json"
	"fmt"
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
		{Request: Request{Method: "GET", Path: "/1/todos/5"}, Response: Response{Status: 200, Body: json.RawMessage(`{"v":"task-2"}`)}, After: []string{"POST /1/todos/5/completion.json"}},
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

	// A later cassette overrides an earlier one, and ".json" is optional.
	// State advances on writes, not on re-reads.
	_, body, _ = get(t, url+"/1/todos/5.json")
	assert.JSONEq(t, `{"v":"task-1"}`, body)
	_, body, _ = get(t, url+"/1/todos/5")
	assert.JSONEq(t, `{"v":"task-1"}`, body)

	// Writes are answered by path and logged with the body sent.
	assert.Equal(t, 204, do(t, "POST", url+"/1/todos/5/completion.json", `{ "a" : 1 }`))
	_, body, _ = get(t, url+"/1/todos/5")
	assert.JSONEq(t, `{"v":"task-2"}`, body)

	// A miss is a 404, logged as unmatched.
	status, _, _ = get(t, url+"/1/todos/999")
	assert.Equal(t, 404, status)

	log := p.Log()
	require.Len(t, log, 7)
	assert.Equal(t, "status=active", log[0].Query)
	assert.True(t, log[4].IsWrite())
	assert.Equal(t, `POST /1/todos/5/completion.json {"a":1}`, log[4].Line())
	assert.False(t, log[6].Matched)
	assert.True(t, log[0].Matched)

	mark := 5
	assert.Len(t, p.Since(mark), 2)
	assert.Equal(t, 7, p.Len())
}

func TestEmptyQueryValueMustBePresent(t *testing.T) {
	p := NewPlayer(&Cassette{Name: "q", Interactions: []Interaction{
		{Request: Request{Method: "GET", Path: "/a"}, Response: Response{Status: 200, Body: json.RawMessage(`"plain"`)}},
		{Request: Request{Method: "GET", Path: "/a", Query: map[string]string{"archived": ""}}, Response: Response{Status: 200, Body: json.RawMessage(`"archived"`)}},
	}})
	url := p.Start()
	defer p.Close()
	_, body, _ := get(t, url+"/a")
	assert.Equal(t, `"plain"`, body)
	_, body, _ = get(t, url+"/a?archived=")
	assert.Equal(t, `"archived"`, body)
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
		"trailing.json": `{"name":"x","interactions":[]} {"name":"y"}`,
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
		"upstream query":     func(p *Profile) { p.Upstream = "https://3.basecampapi.com?tenant=x" },
		"upstream fragment":  func(p *Profile) { p.Upstream = "https://3.basecampapi.com#x" },
		"control in redact":  func(p *Profile) { p.Redact = map[string]string{"A": "B\nC"} },
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
		case r.URL.Path == "/123/netpath":
			w.Header().Set("Location", "//cdn.example/signed?sig=SECRET")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/123/offsite":
			http.Redirect(w, r, "https://cdn.example/signed?sig=SECRET", http.StatusFound)
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
	resp, err = noFollow.Get(url + "/123/offsite")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "an off-origin redirect is refused")
	assert.Empty(t, resp.Header.Get("Location"))
	resp, err = noFollow.Get(url + "/123/netpath")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "a network-path redirect is refused too")

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
	assert.NotContains(t, string(data), "someone%40corp.example")
	assert.NotContains(t, string(data), "SECRET")
	for _, ex := range rec.Since(0) {
		assert.NotContains(t, ex.Query, "someone@corp.example")
		assert.NotContains(t, ex.Query, "someone%40corp.example", "scrubbed before encoding")
	}
	var reads []string
	for _, in := range c.Interactions {
		if in.Request.Path == "/123/thing.json" {
			reads = append(reads, string(in.Response.Body))
		}
	}
	assert.Equal(t, []string{`{"state":"before"}`, `{"state":"after"}`}, reads)

	// Replay walks the same states, advancing on the replay's own write.
	p := NewPlayer(c)
	purl := p.Start()
	defer p.Close()
	_, b1, _ := get(t, purl+"/123/thing.json")
	_, b2, _ := get(t, purl+"/123/thing.json")
	assert.JSONEq(t, `{"state":"before"}`, b1)
	assert.JSONEq(t, `{"state":"before"}`, b2, "re-reading does not advance state")
	do(t, "POST", purl+"/123/thing/touch.json", "")
	_, b3, _ := get(t, purl+"/123/thing.json")
	assert.JSONEq(t, `{"state":"after"}`, b3)
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
	b.Interactions = append(b.Interactions, Interaction{Request: Request{Method: "GET", Path: "/y"}, Response: Response{Status: 200, Body: json.RawMessage(`4`)}, After: []string{"POST /y"}})
	Merge(a, b)
	require.Len(t, a.Interactions, 3, "every recorded state of a new pattern")
	assert.Equal(t, json.RawMessage(`1`), a.Interactions[0].Response.Body)
	assert.Equal(t, "/y", a.Interactions[1].Request.Path)
}

func TestMergeExtendsAPatternsStates(t *testing.T) {
	read := func(v string) Interaction {
		return Interaction{Request: Request{Method: "GET", Path: "/t"}, Response: Response{Status: 200, Body: json.RawMessage(v)}}
	}
	dst := &Cassette{Name: "d", Interactions: []Interaction{read(`"before"`)}}
	after := read(`"after"`)
	after.After = []string{"POST /t/touch"}
	Merge(dst, &Cassette{Name: "s", Interactions: []Interaction{read(`"before-again"`), after}})
	require.Len(t, dst.Interactions, 2)
	assert.Equal(t, json.RawMessage(`"before"`), dst.Interactions[0].Response.Body)
	assert.Equal(t, json.RawMessage(`"after"`), dst.Interactions[1].Response.Body)
}

func TestHeadIsAValidMethod(t *testing.T) {
	c := &Cassette{Name: "h", Interactions: []Interaction{{Request: Request{Method: "HEAD", Path: "/a"}, Response: Response{Status: 200}}}}
	assert.NoError(t, c.Validate())
}

func TestRecorderRefusesForeignAccountsAndSkips404s(t *testing.T) {
	accounts := `[{"id":123,"name":"Seed"}]`
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorization.json":
			_, _ = io.WriteString(w, `{"identity":{"id":1},"accounts":`+accounts+`}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"Not Found"}`)
		}
	}))
	defer upstream.Close()
	t.Setenv("EVAL_REC_TOKEN", "tok")
	rec, err := NewRecorder(&Profile{Name: "seed", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"123"}, TokenEnv: "EVAL_REC_TOKEN"})
	require.NoError(t, err)
	rec.client.Transport = upstream.Client().Transport
	url := rec.Start()
	defer rec.Close()

	status, _, _ := get(t, url+"/authorization.json")
	assert.Equal(t, 200, status)
	status, _, _ = get(t, url+"/123/todos/999")
	assert.Equal(t, 404, status)
	require.Len(t, rec.Cassette("x", "").Interactions, 1, "the 404 is not recorded")

	accounts = `[{"id":123,"name":"Seed"},{"id":456,"name":"Production"}]`
	status, body, _ := get(t, url+"/authorization.json")
	assert.Equal(t, 403, status)
	assert.NotContains(t, body, "Production")
	require.Len(t, rec.Cassette("x", "").Interactions, 1, "nothing recorded from a token that reaches other accounts")
}

func TestReplayStateFollowsTheWritesThatProducedIt(t *testing.T) {
	c := &Cassette{Name: "s", Interactions: []Interaction{
		{Request: Request{Method: "GET", Path: "/1/todos/5"}, Response: Response{Status: 200, Body: json.RawMessage(`"open"`)}},
		{Request: Request{Method: "GET", Path: "/1/todos/5"}, Response: Response{Status: 200, Body: json.RawMessage(`"done"`)}, After: []string{"POST /1/todos/5/completion"}},
		{Request: Request{Method: "GET", Path: "/1/todos/6"}, Response: Response{Status: 200, Body: json.RawMessage(`"created"`)}, After: []string{"POST /1/todos"}},
		{Request: Request{Method: "POST", Path: "/1/todos/5/completion.json"}, Response: Response{Status: 204}},
		{Request: Request{Method: "POST", Path: "/1/todos/9/completion.json"}, Response: Response{Status: 204}},
		{Request: Request{Method: "POST", Path: "/1/todos.json"}, Response: Response{Status: 201, Body: json.RawMessage(`{"id":6}`)}},
	}}
	require.NoError(t, c.Validate())
	p := NewPlayer(c)
	url := p.Start()
	defer p.Close()

	// A resource first recorded after its creation is a miss before it.
	status, _, _ := get(t, url+"/1/todos/6")
	assert.Equal(t, 404, status)
	// An unrelated write does not unlock the completed state.
	do(t, "POST", url+"/1/todos/9/completion.json", "")
	_, body, _ := get(t, url+"/1/todos/5")
	assert.Equal(t, `"open"`, body)
	do(t, "POST", url+"/1/todos/5/completion.json", "")
	_, body, _ = get(t, url+"/1/todos/5")
	assert.Equal(t, `"done"`, body)
	do(t, "POST", url+"/1/todos.json", `{}`)
	status, body, _ = get(t, url+"/1/todos/6")
	assert.Equal(t, 200, status)
	assert.Equal(t, `"created"`, body)

	bad := &Cassette{Name: "b", Interactions: []Interaction{{Request: Request{Method: "GET", Path: "/a"}, Response: Response{Status: 200}, After: []string{"GET /a"}}}}
	assert.Error(t, bad.Validate(), "after names writes only")
}

func TestRecorderKeepsEveryLinkField(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Link", `<https://x/1>; rel="prev"`)
		w.Header().Add("Link", `<https://x/3>; rel="next"`)
		_, _ = io.WriteString(w, `[]`)
	}))
	defer upstream.Close()
	t.Setenv("EVAL_REC_TOKEN", "tok")
	rec, err := NewRecorder(&Profile{Name: "seed", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"123"}, TokenEnv: "EVAL_REC_TOKEN"})
	require.NoError(t, err)
	rec.client.Transport = upstream.Client().Transport
	url := rec.Start()
	defer rec.Close()
	_, _, hdr := get(t, url+"/123/list.json")
	assert.Contains(t, hdr.Get("Link"), `rel="next"`)
	assert.Contains(t, rec.Cassette("x", "").Interactions[0].Response.Headers["Link"], `rel="next"`)
}

func TestRepeatedWritesAreDistinctStatesAndTheTokenIsScrubbed(t *testing.T) {
	comments := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			comments++
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"echo":"`+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+`"}`)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"comments":%d}`, comments))
	}))
	defer upstream.Close()
	t.Setenv("EVAL_REC_TOKEN", "super-secret-token")
	rec, err := NewRecorder(&Profile{Name: "seed", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"123"}, TokenEnv: "EVAL_REC_TOKEN"})
	require.NoError(t, err)
	rec.client.Transport = upstream.Client().Transport
	url := rec.Start()
	defer rec.Close()
	for i := 0; i < 2; i++ {
		do(t, "POST", url+"/123/recordings/1/comments.json", `{}`)
		get(t, url+"/123/recordings/1.json")
	}
	c := rec.Cassette("x", "")
	data, _ := json.Marshal(c)
	assert.NotContains(t, string(data), "super-secret-token")

	p := NewPlayer(c)
	purl := p.Start()
	defer p.Close()
	do(t, "POST", purl+"/123/recordings/1/comments.json", `{}`)
	_, b1, _ := get(t, purl+"/123/recordings/1.json")
	do(t, "POST", purl+"/123/recordings/1/comments.json", `{}`)
	_, b2, _ := get(t, purl+"/123/recordings/1.json")
	assert.JSONEq(t, `{"comments":1}`, b1)
	assert.JSONEq(t, `{"comments":2}`, b2, "the second occurrence of a write is a state of its own")
}

func TestACorrectedRetryIsAnsweredByItsOwnBody(t *testing.T) {
	c := &Cassette{Name: "r", Interactions: []Interaction{
		{Request: Request{Method: "POST", Path: "/1/todos.json", Body: json.RawMessage(`{"due_on":"Friday"}`)}, Response: Response{Status: 422, Body: json.RawMessage(`{"error":"bad date"}`)}},
		{Request: Request{Method: "POST", Path: "/1/todos.json", Body: json.RawMessage(`{"due_on":"2026-10-02"}`)}, Response: Response{Status: 201, Body: json.RawMessage(`{"id":7}`)}},
	}}
	p := NewPlayer(c)
	url := p.Start()
	defer p.Close()
	assert.Equal(t, 422, do(t, "POST", url+"/1/todos.json", `{"due_on":"Friday"}`))
	assert.Equal(t, 201, do(t, "POST", url+"/1/todos.json", `{"due_on": "2026-10-02"}`))
}

func TestRecorderRefusesARedactionThatBreaksJSON(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":123}`)
	}))
	defer upstream.Close()
	t.Setenv("EVAL_REC_TOKEN", "tok")
	rec, err := NewRecorder(&Profile{Name: "seed", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"1"}, TokenEnv: "EVAL_REC_TOKEN", Redact: map[string]string{"123": "fixture"}})
	require.NoError(t, err)
	rec.client.Transport = upstream.Client().Transport
	url := rec.Start()
	defer rec.Close()
	status, _, _ := get(t, url+"/1/thing.json")
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Empty(t, rec.Cassette("x", "").Interactions)
}
