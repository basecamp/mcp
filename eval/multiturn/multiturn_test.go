package multiturn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/eval/cassette"
)

const fakeDir = "../testdata/multiturn/fake"

func loadFake(t *testing.T) (*Corpus, *ArmSet) {
	t.Helper()
	c, err := LoadCorpus(filepath.Join(fakeDir, "tasks.json"))
	require.NoError(t, err)
	a, err := LoadArms(filepath.Join(fakeDir, "arms.json"))
	require.NoError(t, err)
	return c, a
}

func TestScriptRunPassesEveryArm(t *testing.T) {
	c, a := loadFake(t)
	rep, err := Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{ScriptAgent{}}, Launch: ConnectFake})
	require.NoError(t, err)
	require.Len(t, rep.Records, len(c.Tasks)*len(a.Arms))
	require.NoError(t, RequirePass(rep.Records))

	out := rep.Render()
	assert.Contains(t, out, "script    bare")
	assert.Contains(t, out, "script    skill")
	assert.Contains(t, out, "TOTAL COST: $0.0000 over 16 episodes")
}

// badAgent plays fixed calls regardless of task — the shape of an agent that
// gets things wrong, to prove the grader catches it.
type badAgent struct {
	calls  []ScriptCall
	answer string
}

func (badAgent) Label() string   { return "bad" }
func (badAgent) ModelID() string { return "bad" }
func (b badAgent) Run(ctx context.Context, ep *Episode) error {
	ep.Turns = len(b.calls)
	for _, s := range b.calls {
		ep.Call(ctx, s.Tool, s.Arguments)
	}
	ep.Finish(b.answer)
	return nil
}

func runOne(t *testing.T, taskID, arm string, agent Agent) Record {
	t.Helper()
	c, a := loadFake(t)
	require.NoError(t, c.Filter([]string{taskID}))
	require.NoError(t, a.Select([]string{arm}))
	rep, err := Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{agent}, Launch: ConnectFake})
	require.NoError(t, err)
	require.Len(t, rep.Records, 1)
	return rep.Records[0]
}

func todoCall(action string, params map[string]any) ScriptCall {
	return ScriptCall{Tool: "fake_todos", Arguments: map[string]any{"action": action, "params": params}}
}

func TestGradeCatchesPermanentDeleteWhenTrashWasAsked(t *testing.T) {
	r := runOne(t, "trash-stale-todo", "bare", badAgent{calls: []ScriptCall{
		todoCall("delete_todo", map[string]any{"todo_id": 12}),
	}, answer: "Deleted it."})
	assert.False(t, r.Pass)
	assert.Equal(t, 0.0, r.Score)
	assert.Equal(t, 2, r.Safety, "the rejected call and the rejected write both count")
	assert.Contains(t, strings.Join(r.Reasons, "\n"), "missing write")
}

func TestGradeCountsWrongIDsAndWrongTools(t *testing.T) {
	r := runOne(t, "complete-todo", "bare", badAgent{calls: []ScriptCall{
		todoCall("get_todo", map[string]any{"todo_id": 999}),              // not in the cassette: 404
		todoCall("finish_todo", map[string]any{"todo_id": 11}),            // unknown action
		{Tool: "fake_guide", Arguments: map[string]any{}},                 // hidden in the bare arm
		todoCall("complete_todo", map[string]any{"todo_id": float64(11)}), // right
	}, answer: "Done."})
	assert.True(t, r.Pass, r.Reasons)
	assert.Equal(t, 1, r.WrongID)
	assert.Equal(t, 2, r.WrongTool)
	assert.Equal(t, 1, r.GuideCalls)
	assert.Equal(t, 4, r.Calls)
	require.Len(t, r.Trace, 4)
	assert.True(t, r.Trace[2].Unknown)
	assert.False(t, r.Trace[0].Requests[0].Matched)
}

func TestGradeFlagsAnAttemptedRejectedWriteEvenWhenReplayCannotAnswer(t *testing.T) {
	// complete-todo's cassette answers only todo 11's completion; completing
	// 12 is refused by the replay, but the attempt still trips the rule.
	r := runOne(t, "complete-todo", "bare", badAgent{calls: []ScriptCall{
		todoCall("complete_todo", map[string]any{"todo_id": 13}),
		todoCall("complete_todo", map[string]any{"todo_id": 11}),
	}, answer: "Done."})
	assert.False(t, r.Pass)
	assert.Equal(t, 1, r.Safety)
	assert.Equal(t, 1, r.WrongID)
}

func TestGradeFlagsAnyWriteOnAReadOnlyTask(t *testing.T) {
	r := runOne(t, "open-todos", "bare", badAgent{calls: []ScriptCall{
		todoCall("list_todos", map[string]any{"project_id": 1}),
		todoCall("complete_todo", map[string]any{"todo_id": 11}),
		todoCall("delete_todo", map[string]any{"todo_id": 99}), // unanswered: still attempted
	}, answer: "Write launch notes and the press release draft."})
	assert.False(t, r.Pass)
	assert.Equal(t, 2, r.Safety)
}

func TestGradeWantsTheAnswer(t *testing.T) {
	r := runOne(t, "open-todos", "bare", badAgent{calls: []ScriptCall{
		todoCall("list_todos", map[string]any{"project_id": 1}),
	}, answer: "Nothing much."})
	assert.False(t, r.Pass)
	assert.InDelta(t, 1.0/3, r.Score, 0.001)
}

func TestArmsRealize(t *testing.T) {
	_, a := loadFake(t)
	ctx := context.Background()
	byName := map[string]Arm{}
	for _, arm := range a.Arms {
		byName[arm.Name] = arm
	}
	surface := func(arm Arm) (*Surface, error) {
		session, cleanup, err := ConnectFake(ctx, arm, "http://127.0.0.1:1")
		require.NoError(t, err)
		defer cleanup()
		return a.Realize(ctx, session, arm)
	}
	names := func(s *Surface) []string {
		var out []string
		for _, tool := range s.Tools {
			out = append(out, tool.Name)
		}
		return out
	}

	bare, err := surface(byName["bare"])
	require.NoError(t, err)
	assert.Empty(t, bare.Instructions)
	assert.Empty(t, bare.Skill)
	assert.NotContains(t, names(bare), "fake_guide")

	skill, err := surface(byName["skill"])
	require.NoError(t, err)
	assert.NotEmpty(t, skill.Instructions)
	assert.Contains(t, skill.Skill, "Prefer trash_todo")
	assert.Contains(t, names(skill), "fake_guide")
	sys := SystemPrompt(skill, "2026-09-29")
	assert.Contains(t, sys, "Today's date is 2026-09-29")
	assert.Contains(t, sys, "# MCP server instructions")
	assert.Contains(t, sys, "# Skill")

	// The client side holds the line even when the server offers more: a
	// server launched with every flag still yields a bare surface for the
	// bare arm.
	full := Arm{Name: "bare", ServerArgs: []string{"--instructions", "--guide", "--skill"}}
	s, err := surface(full)
	require.NoError(t, err)
	assert.Empty(t, s.Instructions)
	assert.NotContains(t, names(s), "fake_guide")
	assert.NotContains(t, SystemPrompt(s, ""), "# MCP server instructions")

	// An arm asking for guidance the server does not offer is refused.
	_, err = surface(Arm{Name: "x", Instructions: true})
	assert.ErrorContains(t, err, "sent none")
	_, err = surface(Arm{Name: "x", Guide: true})
	assert.ErrorContains(t, err, "does not list fake_guide")
	_, err = surface(Arm{Name: "x", Skill: true})
	assert.Error(t, err)
}

func TestRunRefusesAnUnrealizableArmBeforeAnyEpisode(t *testing.T) {
	c, a := loadFake(t)
	a.Arms = []Arm{{Name: "guide-without-flag", Instructions: true, Guide: true, ServerArgs: []string{"--instructions"}}}
	var ran bool
	agent := agentFunc(func(context.Context, *Episode) error { ran = true; return nil })
	_, err := Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{agent}, Launch: ConnectFake})
	assert.ErrorContains(t, err, "does not list fake_guide")
	assert.False(t, ran)
}

type agentFunc func(context.Context, *Episode) error

func (agentFunc) Label() string                                { return "fn" }
func (agentFunc) ModelID() string                              { return "fn" }
func (f agentFunc) Run(ctx context.Context, ep *Episode) error { return f(ctx, ep) }

func TestNormalize(t *testing.T) {
	op, params := normalize("basecamp_todos", map[string]any{"action": "get_todo", "params": map[string]any{"todo_id": 1}})
	assert.Equal(t, "get_todo", op)
	assert.Equal(t, map[string]any{"todo_id": 1}, params)

	op, params = normalize("basecamp", map[string]any{"action": "search", "query": "x"})
	assert.Equal(t, "search", op)
	assert.Equal(t, map[string]any{"query": "x"}, params)

	op, params = normalize("get_todo", map[string]any{"todo_id": 1})
	assert.Equal(t, "get_todo", op)
	assert.Equal(t, map[string]any{"todo_id": 1}, params)

	assert.Equal(t, `get_todo {"a":1,"b":2}`, Step{Op: "get_todo", Params: map[string]any{"b": 2, "a": 1}}.Line())
	assert.Equal(t, `overview {}`, Step{Op: "overview"}.Line())
}

func TestCorpusValidation(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cassettes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cassettes", "c.json"), []byte(`{"name":"c","interactions":[]}`), 0o644))
	good := map[string]any{
		"id": "t", "prompt": "p", "cassettes": []any{"c"},
		"expect": map[string]any{"writes": []any{"^POST "}},
		"script": []any{map[string]any{"tool": "x", "arguments": map[string]any{}}},
	}
	load := func(mutate func(task map[string]any), top map[string]any) error {
		task := map[string]any{}
		for k, v := range good {
			task[k] = v
		}
		if mutate != nil {
			mutate(task)
		}
		doc := map[string]any{"server": "s", "cassette_dir": "cassettes", "max_turns": 3, "tasks": []any{task}}
		for k, v := range top {
			doc[k] = v
		}
		data, _ := json.Marshal(doc)
		path := filepath.Join(dir, "tasks.json")
		require.NoError(t, os.WriteFile(path, data, 0o644))
		_, err := LoadCorpus(path)
		return err
	}
	require.NoError(t, load(nil, nil))

	// A missing cassette loads (a recording run creates it) but fails the
	// replay check.
	require.NoError(t, load(func(t map[string]any) { t["cassettes"] = []any{"nope"} }, nil))
	c, err := LoadCorpus(filepath.Join(dir, "tasks.json"))
	require.NoError(t, err)
	assert.ErrorContains(t, c.CheckCassettes(), "nope")
	for name, m := range map[string]func(map[string]any){
		"no id":              func(t map[string]any) { t["id"] = "" },
		"path id":            func(t map[string]any) { t["id"] = "../tasks" },
		"no prompt":          func(t map[string]any) { t["prompt"] = " " },
		"no cassettes":       func(t map[string]any) { t["cassettes"] = []any{} },
		"expects nothing":    func(t map[string]any) { t["expect"] = map[string]any{} },
		"write without read": func(t map[string]any) { t["expect"] = map[string]any{"answer": []any{"x"}} },
		"read_only writes":   func(t map[string]any) { t["read_only"] = true },
		"bad regex":          func(t map[string]any) { t["expect"] = map[string]any{"writes": []any{"("}} },
		"vacuous regex":      func(t map[string]any) { t["expect"] = map[string]any{"writes": []any{"(?i)"}} },
		"no script":          func(t map[string]any) { delete(t, "script") },
		"negative turns":     func(t map[string]any) { t["max_turns"] = -1 },
		"unknown field":      func(t map[string]any) { t["expekt"] = 1 },
	} {
		assert.Error(t, load(m, nil), name)
	}
	assert.Error(t, load(nil, map[string]any{"tasks": []any{}}), "empty corpus")
	assert.Error(t, load(nil, map[string]any{"max_turns": 0}), "no turn budget")
	assert.Error(t, load(nil, map[string]any{"tasks": []any{good, good}}), "duplicate id")
}

// TestCommittedCorporaLoad keeps every committed corpus, arms file and
// cassette loadable, so a hand edit that breaks one fails here rather than in
// a paid run.
func TestCommittedCorporaLoad(t *testing.T) {
	for _, server := range []string{"fake", "basecamp"} {
		dir := filepath.Join("..", "testdata", "multiturn", server)
		c, err := LoadCorpus(filepath.Join(dir, "tasks.json"))
		require.NoError(t, err, server)
		assert.Equal(t, server, c.Server)
		a, err := LoadArms(filepath.Join(dir, "arms.json"))
		require.NoError(t, err, server)
		assert.Equal(t, server, a.Server)
		for _, task := range c.Tasks {
			for _, name := range task.Cassettes {
				_, err := cassette.Load(c.CassettePath(name))
				require.NoError(t, err, "%s/%s", task.ID, name)
			}
		}
	}
}

// TestBasecampCassettesCarryNoSecrets guards the hand-authored fixtures the
// way the recorder guards recorded ones: fictional ids and example.com
// addresses only, no bearer tokens, no live API origin.
func TestBasecampCassettesCarryNoSecrets(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "testdata", "multiturn", "*", "cassettes", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		s := string(data)
		for _, bad := range []string{"basecampapi.com", "Bearer ", "access_token", "37signals"} {
			assert.NotContains(t, s, bad, f)
		}
		for _, email := range emailRE.FindAllString(s, -1) {
			assert.True(t, strings.HasSuffix(email, "@example.com"), "%s: %s", f, email)
		}
	}
}

var emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// fakeAnthropic scripts Messages API responses and records requests.
type fakeAnthropic struct {
	mu        sync.Mutex
	responses []string
	requests  []map[string]any
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		w.WriteHeader(500)
		return
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	_, _ = io.Copy(w, bytes.NewBufferString(resp))
}

func TestAPIAgentLoop(t *testing.T) {
	fa := &fakeAnthropic{responses: []string{
		`{"content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"Looking."},{"type":"tool_use","id":"tu1","name":"fake_projects","input":{"action":"list_projects"}},{"type":"tool_use","id":"tu2","name":"fake_todos","input":{"action":"list_todos","params":{"project_id":1}}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":1000,"cache_read_input_tokens":0}}`,
		`{"content":[{"type":"tool_use","id":"tu3","name":"fake_todos","input":{"action":"complete_todo","params":{"todo_id":11}}}],"stop_reason":"tool_use","usage":{"input_tokens":50,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000}}`,
		`{"content":[{"type":"text","text":"Marked it done."}],"stop_reason":"end_turn","usage":{"input_tokens":40,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000}}`,
	}}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	agent, err := NewAPIAgent("haiku", ModelID("haiku"))
	require.NoError(t, err)

	r := runOne(t, "complete-todo", "guide", agent)
	assert.True(t, r.Pass, r.Reasons)
	assert.Equal(t, 3, r.Turns)
	assert.Equal(t, 3, r.Calls)
	assert.Equal(t, "Marked it done.", r.Answer)
	assert.Equal(t, 190, r.InTokens)
	assert.Equal(t, 35, r.OutTokens)
	assert.Equal(t, 2000, r.CacheReadTokens)
	// haiku-4-5: $1 in / $5 out; cache write 1.25x, read 0.1x.
	want := (190*1.0 + 1000*1.25 + 2000*0.1 + 35*5.0) / 1e6
	assert.InDelta(t, want, r.CostUSD, 1e-12)
	assert.False(t, r.PricingEstimated)

	require.Len(t, fa.requests, 3)
	first := fa.requests[0]
	assert.Equal(t, "claude-haiku-4-5-20251001", first["model"])
	assert.Contains(t, first["system"], "# MCP server instructions")
	var toolNames []string
	for _, tool := range first["tools"].([]any) {
		toolNames = append(toolNames, tool.(map[string]any)["name"].(string))
	}
	assert.ElementsMatch(t, []string{"fake_projects", "fake_todos", "fake_guide"}, toolNames)

	// Turn two carries the verbatim assistant turn (thinking included) and
	// both tool results in one user message.
	msgs := fa.requests[1]["messages"].([]any)
	require.Len(t, msgs, 3)
	assistant := msgs[1].(map[string]any)["content"].([]any)
	assert.Equal(t, "thinking", assistant[0].(map[string]any)["type"])
	results := msgs[2].(map[string]any)["content"].([]any)
	require.Len(t, results, 2)
	assert.Equal(t, "tu1", results[0].(map[string]any)["tool_use_id"])
	assert.Contains(t, results[1].(map[string]any)["content"], "Write launch notes")
}

func TestAPIAgentExhaustsTurnBudget(t *testing.T) {
	loop := `{"content":[{"type":"tool_use","id":"tu","name":"fake_projects","input":{"action":"list_projects"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	fa := &fakeAnthropic{responses: []string{loop, loop, loop, loop, loop, loop, loop, loop, loop}}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	agent, err := NewAPIAgent("some-new-model", "some-new-model")
	require.NoError(t, err)
	r := runOne(t, "complete-todo", "bare", agent)
	assert.True(t, r.Exhausted)
	assert.False(t, r.Pass)
	assert.Equal(t, 8, r.Turns, "the fake corpus budget")
	assert.True(t, r.PricingEstimated, "an unpriced model id is marked estimated")
}

func TestAPIAgentSurfacesAPIErrors(t *testing.T) {
	fa := &fakeAnthropic{responses: []string{`{"type":"error","error":{"type":"invalid_request_error","message":"bad tool schema"}}`}}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	agent, err := NewAPIAgent("haiku", ModelID("haiku"))
	require.NoError(t, err)
	r := runOne(t, "complete-todo", "bare", agent)
	assert.Contains(t, r.Error, "bad tool schema")
	assert.False(t, r.Pass)
	assert.Error(t, RequirePass([]Record{r}))
}

func TestScriptAgentFailsOnABrokenGoldPath(t *testing.T) {
	c, a := loadFake(t)
	require.NoError(t, c.Filter([]string{"open-todos"}))
	require.NoError(t, a.Select([]string{"bare"}))
	c.Tasks[0].Script[1].Arguments = map[string]any{"action": "list_todos", "params": map[string]any{"project_id": 404}}
	rep, err := Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{ScriptAgent{}}, Launch: ConnectFake})
	require.NoError(t, err)
	r := rep.Records[0]
	assert.False(t, r.Pass, "the canned answer alone must not pass")
	assert.Contains(t, r.Error, "gold script failed: step 2")
}

func TestFilterRejectsDuplicates(t *testing.T) {
	c, _ := loadFake(t)
	assert.ErrorContains(t, c.Filter([]string{"add-todo", "add-todo"}), "twice")
}

func TestAPIAgentRejectsATruncatedTurn(t *testing.T) {
	fa := &fakeAnthropic{responses: []string{`{"content":[{"type":"text","text":"Marked it do"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`}}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	agent, err := NewAPIAgent("haiku", ModelID("haiku"))
	require.NoError(t, err)
	r := runOne(t, "complete-todo", "bare", agent)
	assert.False(t, r.Pass)
	assert.Contains(t, r.Error, "max_tokens")
}

// TestRecordThenReplay records a task through the recorder against a live-ish
// upstream (the fake world served over TLS), then replays the recorded
// cassette, and checks the recording run's results carry no free text.
func TestRecordThenReplay(t *testing.T) {
	base, err := cassette.Load(filepath.Join(fakeDir, "cassettes", "base.json"))
	require.NoError(t, err)
	world := cassette.NewPlayer(base)
	upstream := httptest.NewTLSServer(world)
	defer upstream.Close()
	world.SetBase(upstream.URL)
	prev := http.DefaultTransport
	http.DefaultTransport = upstream.Client().Transport
	defer func() { http.DefaultTransport = prev }()

	dir := t.TempDir()
	t.Setenv("EVAL_RT_TOKEN", "tok")
	c, a := loadFake(t)
	require.NoError(t, c.Filter([]string{"complete-todo"}))
	require.NoError(t, a.Select([]string{"bare"}))
	c.Tasks[0].Cassettes = []string{"complete-todo"}
	prof := &cassette.Profile{Name: "rt", TestAccount: true, Upstream: upstream.URL, AccountIDs: []string{"1"}, TokenEnv: "EVAL_RT_TOKEN"}
	rep, err := Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{ScriptAgent{}}, Launch: ConnectFake, Record: prof, RecordDir: dir})
	require.NoError(t, err)
	r := rep.Records[0]
	assert.True(t, r.Pass, r.Reasons)
	assert.Nil(t, r.Trace, "a recording's results carry no trace")
	assert.Empty(t, r.Answer)

	// Replay the recording from its own directory.
	c.CassetteDir = ""
	recorded, err := cassette.Load(filepath.Join(dir, "complete-todo.json"))
	require.NoError(t, err)
	require.NotEmpty(t, recorded.Interactions)
	c2 := *c
	c2.dir = dir
	rep, err = Run(context.Background(), Config{Corpus: &c2, Arms: a, Agents: []Agent{ScriptAgent{}}, Launch: ConnectFake})
	require.NoError(t, err)
	assert.True(t, rep.Records[0].Pass, rep.Records[0].Reasons)
	assert.NotNil(t, rep.Records[0].Trace)

	// An episode that errors part-way saves nothing, and the run fails.
	dir2 := t.TempDir()
	failing := agentFunc(func(ctx context.Context, ep *Episode) error {
		ep.Call(ctx, "fake_projects", map[string]any{"action": "list_projects"})
		return fmt.Errorf("model went away")
	})
	rep, err = Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{failing}, Launch: ConnectFake, Record: prof, RecordDir: dir2})
	assert.ErrorContains(t, err, "nothing saved")
	assert.ErrorContains(t, err, "model went away", "the operator gets the detail")
	assert.NotContains(t, rep.Records[0].Error, "model went away", "the results file does not")
	_, statErr := os.Stat(filepath.Join(dir2, "complete-todo.json"))
	assert.True(t, os.IsNotExist(statErr))

	// More than one task is refused before anything runs.
	c3, _ := loadFake(t)
	_, err = Run(context.Background(), Config{Corpus: c3, Arms: a, Agents: []Agent{ScriptAgent{}}, Launch: ConnectFake, Record: prof, RecordDir: dir})
	assert.ErrorContains(t, err, "one task")
}

func TestSkillFileSymlinkMayNotLeaveTheArmsDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.env")
	require.NoError(t, os.WriteFile(outside, []byte("TOKEN=x"), 0o644))
	dir := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "skill.md")))
	path := filepath.Join(dir, "arms.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"server":"s","guide_tools":[],"skill_file":"skill.md","arms":[{"name":"skill","skill":true}]}`), 0o644))
	a, err := LoadArms(path)
	require.NoError(t, err)
	_, err = a.skill(context.Background(), nil)
	assert.ErrorContains(t, err, "resolves outside")
}

func TestSkillFileStaysBesideTheArmsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arms.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"server":"s","guide_tools":[],"skill_file":"../../.env","arms":[{"name":"skill","skill":true}]}`), 0o644))
	_, err := LoadArms(path)
	assert.ErrorContains(t, err, "beneath")
}

// truncatingAnthropic claims a longer body than it sends: a complete JSON
// prefix, then the connection ends.
func TestAPIAgentRejectsATruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	agent, err := NewAPIAgent("haiku", ModelID("haiku"))
	require.NoError(t, err)
	agent.client.Timeout = 5 * time.Second
	agent.backoff = time.Millisecond
	r := runOne(t, "open-todos", "bare", agent)
	assert.False(t, r.Pass)
	assert.Contains(t, r.Error, "read response")
}

func TestAPIAgentCountsARefusalsUsage(t *testing.T) {
	fa := &fakeAnthropic{responses: []string{`{"content":[],"stop_reason":"refusal","usage":{"input_tokens":1000,"output_tokens":10}}`}}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	agent, err := NewAPIAgent("haiku", ModelID("haiku"))
	require.NoError(t, err)
	r := runOne(t, "complete-todo", "bare", agent)
	assert.Contains(t, r.Error, "refused")
	assert.Equal(t, 1000, r.InTokens)
	assert.Greater(t, r.CostUSD, 0.0)
}

// TestCommittedResultsAreBaselines keeps every committed results file
// loadable as a --baseline: identity complete, no duplicate cells.
func TestCommittedResultsAreBaselines(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "results", "multiturn", "*.jsonl"))
	require.NoError(t, err)
	files = append(files, filepath.Join(fakeDir, "baseline-script.jsonl"))
	for _, f := range files {
		fh, err := os.Open(f)
		require.NoError(t, err)
		_, err = LoadBaseline(fh)
		_ = fh.Close()
		require.NoError(t, err, f)
	}
}

type logBackend []cassette.Exchange

func (l logBackend) Len() int { return len(l) }
func (l logBackend) Since(mark int) []cassette.Exchange {
	if mark >= len(l) {
		return nil
	}
	return l[mark:]
}

func TestGradeSeesWritesOutsideToolCallsAndOnlySuccessfulExpectedCalls(t *testing.T) {
	read := cassette.Exchange{Method: "GET", Path: "/projects/1/todos.json", Status: 200, Matched: true}
	sneaky := cassette.Exchange{Method: "POST", Path: "/todos/11/completion.json", Status: 204, Matched: true}
	ep := &Episode{
		Task: Task{ID: "t", ReadOnly: true, Expect: Expect{Calls: []string{"^list_todos "}}},
		Steps: []Step{
			{Tool: "fake_todos", Op: "list_todos", Params: map[string]any{"project_id": 1}, IsError: true}, // schema-invalid: counts for nothing
			{Tool: "fake_todos", Op: "get_todo", Params: map[string]any{"todo_id": 11}, Requests: []cassette.Exchange{read}},
		},
		backend:      logBackend{read, sneaky, sneaky}, // outside any call, twice
		attributedAt: map[int]bool{0: true},
	}
	r := grade(ep, func(string) bool { return false })
	assert.False(t, r.Pass)
	assert.Equal(t, 2, r.Safety, "each write outside tool calls on a read-only task, by occurrence")
	ep.backend = logBackend{read, cassette.Exchange{Method: "GET", Path: "/nope", Status: 404}}
	assert.Equal(t, 1, grade(ep, func(string) bool { return false }).WrongID, "a miss outside tool calls counts too")
	ep.backend = logBackend{read, cassette.Exchange{Method: "POST", Path: "/f", Status: 415}}
	assert.Equal(t, 0, grade(ep, func(string) bool { return false }).WrongID, "a refused malformed request is not a wrong id")
	assert.Contains(t, strings.Join(r.Reasons, "\n"), "missing call", "the failed list_todos does not satisfy expect.calls")
}

func TestPaidRunsProveGoldScriptsFirst(t *testing.T) {
	c, a := loadFake(t)
	require.NoError(t, c.Filter([]string{"open-todos"}))
	require.NoError(t, a.Select([]string{"bare"}))
	c.Tasks[0].Script[1].Arguments = map[string]any{"action": "list_todos", "params": map[string]any{"project_id": 404}}
	var ran bool
	agent := agentFunc(func(context.Context, *Episode) error { ran = true; return nil })
	_, err := Run(context.Background(), Config{Corpus: c, Arms: a, Agents: []Agent{agent}, Launch: ConnectFake})
	assert.ErrorContains(t, err, "gold script for open-todos")
	assert.False(t, ran, "no paid episode after a broken gold path")
}

func TestDigestCoversTheEffectiveBudget(t *testing.T) {
	c, _ := loadFake(t)
	before := c.Tasks[0].Digest()
	path := filepath.Join(t.TempDir(), "tasks.json")
	data, err := os.ReadFile(filepath.Join(fakeDir, "tasks.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(data), `"max_turns": 8`, `"max_turns": 9`, 1)), 0o644))
	c2, err := LoadCorpus(path)
	require.NoError(t, err)
	assert.NotEqual(t, before, c2.Tasks[0].Digest(), "a changed corpus default budget changes the task")
}
