package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/eval/multiturn"
)

// The command runs from the repo root; tests run from the package dir.
func chdirRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(filepath.Join("..", "..", "..")))
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func TestPreflight(t *testing.T) {
	chdirRoot(t)
	out := filepath.Join(t.TempDir(), "out.jsonl")
	base := options{server: "fake", backend: "script", out: out}

	cfg, plan, b, err := preflight(&base)
	require.NoError(t, err)
	assert.Nil(t, b)
	assert.Equal(t, map[string]string{"script": "script"}, plan)
	assert.Len(t, cfg.Corpus.Tasks, 4)
	assert.Len(t, cfg.Arms.Arms, 4)

	cases := map[string]func(o *options){
		"unknown backend": func(o *options) { o.backend = "magic" },
		"api without key": func(o *options) { o.backend = "api"; t.Setenv("ANTHROPIC_API_KEY", "") },
		"duplicate model": func(o *options) { o.backend = "cli"; o.models = "haiku,haiku" },
		"corpus server":   func(o *options) { o.tasks = "eval/testdata/multiturn/basecamp/tasks.json" },
		"unknown arm":     func(o *options) { o.arms = "bare,nope" },
		"unknown task":    func(o *options) { o.only = "nope" },
		"unknown server": func(o *options) {
			o.server = "nope"
			o.tasks = "eval/testdata/multiturn/fake/tasks.json"
			o.armsFile = "eval/testdata/multiturn/fake/arms.json"
		},
		"fake with server-cmd": func(o *options) { o.serverCmd = "x" },
		"out is baseline": func(o *options) {
			o.baseline = "eval/testdata/multiturn/fake/baseline-script.jsonl"
			o.out = o.baseline
		},
		"baseline other models": func(o *options) {
			o.backend = "cli"
			o.models = "haiku"
			o.baseline = "eval/testdata/multiturn/fake/baseline-script.jsonl"
		},
		"record with baseline": func(o *options) {
			o.recordProfile = "x.json"
			o.baseline = "eval/testdata/multiturn/fake/baseline-script.jsonl"
		},
		"record without profile file": func(o *options) { o.recordProfile = "does-not-exist.json" },
		"out is the corpus":           func(o *options) { o.out = "eval/testdata/multiturn/fake/tasks.json" },
		"out is a cassette":           func(o *options) { o.out = "eval/testdata/multiturn/fake/cassettes/base.json" },
		"out is a directory":          func(o *options) { o.out = "eval/testdata" },
	}
	for name, mutate := range cases {
		o := base
		mutate(&o)
		_, _, _, err := preflight(&o)
		assert.Error(t, err, name)
	}

	o := base
	o.baseline = "eval/testdata/multiturn/fake/baseline-script.jsonl"
	_, _, b, err = preflight(&o)
	require.NoError(t, err)
	assert.NotNil(t, b)
}

func TestSortedLabels(t *testing.T) {
	assert.Equal(t, []string{"haiku", "sonnet", "opus", "a-raw-id"},
		sortedLabels(map[string]string{"opus": "", "a-raw-id": "", "sonnet": "", "haiku": ""}))
}

func TestLauncherHonorsQuotes(t *testing.T) {
	_, err := launcher("basecamp", `"/path with spaces/basecamp-mcp" stdio`)
	assert.NoError(t, err)
	_, err = launcher("basecamp", `"unbalanced stdio`)
	assert.Error(t, err)
}

func TestPreflightRecording(t *testing.T) {
	chdirRoot(t)
	dir := t.TempDir()
	prof := filepath.Join(dir, "profile.json")
	require.NoError(t, os.WriteFile(prof, []byte(`{"name":"seed","test_account":true,"upstream":"https://example.invalid","account_ids":["1"],"token_env":"EVAL_PF_TOKEN"}`), 0o644))
	t.Setenv("EVAL_PF_TOKEN", "tok")
	o := options{server: "fake", backend: "script", out: filepath.Join(dir, "out.jsonl"), recordProfile: prof, recordDir: dir}

	_, _, _, err := preflight(&o)
	assert.ErrorContains(t, err, "one task, one model, one arm", "every arm would repeat the live writes")

	o.arms = "bare"
	_, _, _, err = preflight(&o)
	assert.ErrorContains(t, err, "one task", "every task would record the state the last one left")

	o.only = "add-todo"
	cfg, _, _, err := preflight(&o)
	require.NoError(t, err)
	assert.NotNil(t, cfg.Record)

	// A corrupt cassette already at the target fails before any live write.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "add-todo.json"), []byte("{"), 0o644))
	_, _, _, err = preflight(&o)
	assert.ErrorContains(t, err, "existing recording target")
	require.NoError(t, os.Remove(filepath.Join(dir, "add-todo.json")))

	t.Setenv("EVAL_PF_TOKEN", "")
	_, _, _, err = preflight(&o)
	assert.ErrorContains(t, err, "EVAL_PF_TOKEN")
}

func TestPreflightResolvesTheDefaultOutput(t *testing.T) {
	chdirRoot(t)
	o := options{server: "fake", backend: "script"}
	_, _, _, err := preflight(&o)
	require.NoError(t, err)
	assert.Equal(t, "eval/results/multiturn/fake.jsonl", o.out, "the caller writes where preflight checked")
	_ = os.Remove(o.out)
}

func TestLauncherRefusesReservedServerEnv(t *testing.T) {
	launch, err := launcher("basecamp", "/bin/true stdio")
	require.NoError(t, err)
	_, _, err = launch(t.Context(), multiturn.Arm{Name: "x", ServerEnv: map[string]string{"BASECAMP_BASE_URL=https://evil.example/": "v"}}, "http://127.0.0.1:1")
	assert.ErrorContains(t, err, "not a variable name")
	for _, k := range []string{"HOME", "BASECAMP_BASE_URL", "BASECAMP_TOKEN"} {
		_, _, err := launch(t.Context(), multiturn.Arm{Name: "x", ServerEnv: map[string]string{k: "v"}}, "http://127.0.0.1:1")
		assert.ErrorContains(t, err, "may not set "+k)
	}
}

func TestRecordDirAliasIsCaught(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(real, alias))
	assert.True(t, sameDir(alias, real))
	wd, _ := os.Getwd()
	rel, err := filepath.Rel(wd, real)
	require.NoError(t, err)
	assert.True(t, sameDir(rel, real), "relative and absolute spellings of one directory")
	assert.False(t, sameDir(t.TempDir(), real))
}
