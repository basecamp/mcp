// Command multiturn runs the eval's multi-turn mode: an agent loop over a real
// MCP server, replayed against cassettes, across guidance arms, graded by rule
// over the whole trace. See eval/README.md ("Multi-turn mode").
//
// Usage:
//
//	# CI smoke: in-process fake server, gold scripts, no model, no network.
//	go run ./eval/cmd/multiturn --server fake --backend script --require-pass \
//	    --baseline eval/testdata/multiturn/fake/baseline-script.jsonl
//
//	# basecamp-mcp over stdio, replayed; the gold scripts prove the cassettes.
//	go run ./eval/cmd/multiturn --server basecamp --server-cmd "/tmp/basecamp-mcp stdio" \
//	    --backend script --arms bare
//
//	# Real models (ANTHROPIC_API_KEY), every arm.
//	go run ./eval/cmd/multiturn --server basecamp --server-cmd "/tmp/basecamp-mcp stdio" \
//	    --backend api --models haiku,sonnet
//
//	# Real models through the local claude CLI as MCP host (no API key).
//	go run ./eval/cmd/multiturn --server basecamp --server-cmd "/tmp/basecamp-mcp stdio" \
//	    --backend cli --models haiku,sonnet --arms bare,instructions --parallel 4
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/eval/cassette"
	"github.com/basecamp/mcp/eval/multiturn"
)

var errRegression = errors.New("baseline regression detected")

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "multiturn: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	server, serverCmd, tasks, armsFile, arms, only, models, backend string
	out, baseline, recordProfile, recordDir                         string
	maxTurns, parallel                                              int
	requirePass, quiet                                              bool
}

func run() error {
	// Hidden mode: the process the claude CLI spawns as its MCP server under
	// --backend cli, relaying stdio to the episode's surface socket.
	if len(os.Args) == 3 && os.Args[1] == "--bridge" {
		return multiturn.Bridge(os.Args[2], os.Stdin, os.Stdout)
	}
	var o options
	flag.StringVar(&o.server, "server", "fake", "server under test: fake (in-process) or basecamp")
	flag.StringVar(&o.serverCmd, "server-cmd", "", "command to spawn the stdio server (default: the registry's binary on PATH, or EVAL_<SERVER>_CMD)")
	flag.StringVar(&o.tasks, "tasks", "", "task corpus (default eval/testdata/multiturn/<server>/tasks.json)")
	flag.StringVar(&o.armsFile, "arms-file", "", "arms file (default eval/testdata/multiturn/<server>/arms.json)")
	flag.StringVar(&o.arms, "arms", "", "comma-separated arms to run (default: every arm in the arms file)")
	flag.StringVar(&o.only, "only", "", "comma-separated task ids to run (default: all)")
	flag.StringVar(&o.models, "models", "haiku,sonnet", "comma-separated model labels (haiku, sonnet, opus) or raw model ids; ignored by --backend script")
	flag.StringVar(&o.backend, "backend", "script", "agent backend: script (gold scripts, no spend), api (Anthropic Messages API), or cli (the local claude CLI as MCP host; no API key)")
	flag.IntVar(&o.maxTurns, "max-turns", 0, "override every task's turn budget")
	flag.IntVar(&o.parallel, "parallel", 1, "episodes to run at once (each has its own backend and server)")
	flag.StringVar(&o.out, "out", "", "JSONL results path (default eval/results/<server>-multiturn.jsonl)")
	flag.StringVar(&o.baseline, "baseline", "", "compare against a prior results JSONL; exit nonzero on a newly-failing task, score drop, or new safety violation")
	flag.BoolVar(&o.requirePass, "require-pass", false, "exit nonzero unless every episode passes (for the script smoke)")
	flag.StringVar(&o.recordProfile, "record-profile", "", "RECORD against a live test account through this profile instead of replaying (see eval/README.md)")
	flag.StringVar(&o.recordDir, "record-dir", "", "directory recorded cassettes are written to (with --record-profile)")
	flag.BoolVar(&o.quiet, "quiet", false, "suppress per-episode progress lines")
	flag.Parse()

	ctx := context.Background()
	cfg, plan, base, err := preflight(o)
	if err != nil {
		return err
	}
	agents, err := buildAgents(o.backend, plan)
	if err != nil {
		return err
	}
	cfg.Agents = agents
	if !o.quiet {
		cfg.Progress = os.Stderr
	}

	rep, err := multiturn.Run(ctx, cfg)
	if err != nil {
		return err
	}

	f, err := os.Create(o.out)
	if err != nil {
		return err
	}
	if err := multiturn.WriteJSONL(f, rep.Records); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Print(rep.Render())
	fmt.Fprintf(os.Stderr, "\nwrote %d records to %s\n", len(rep.Records), o.out)

	if base != nil {
		cmp, err := multiturn.Compare(base, rep.Records)
		if err != nil {
			return err
		}
		fmt.Print(cmp.Render(o.baseline))
		if cmp.HasRegression() {
			return errRegression
		}
	}
	if o.requirePass {
		if err := multiturn.RequirePass(rep.Records); err != nil {
			return fmt.Errorf("--require-pass: %w", err)
		}
	}
	return nil
}

// preflight resolves and checks everything that needs neither a server nor a
// paid call: files, flags, the API key, output paths, the baseline and its
// overlap with this run.
func preflight(o options) (multiturn.Config, map[string]string, *multiturn.Baseline, error) {
	var cfg multiturn.Config
	fail := func(err error) (multiturn.Config, map[string]string, *multiturn.Baseline, error) {
		return cfg, nil, nil, err
	}
	if o.tasks == "" {
		o.tasks = fmt.Sprintf("eval/testdata/multiturn/%s/tasks.json", o.server)
	}
	if o.armsFile == "" {
		o.armsFile = fmt.Sprintf("eval/testdata/multiturn/%s/arms.json", o.server)
	}
	corpus, err := multiturn.LoadCorpus(o.tasks)
	if err != nil {
		return fail(err)
	}
	if corpus.Server != o.server {
		return fail(fmt.Errorf("corpus %s is for server %q but --server is %q", o.tasks, corpus.Server, o.server))
	}
	if err := corpus.Filter(splitCSV(o.only)); err != nil {
		return fail(err)
	}
	arms, err := multiturn.LoadArms(o.armsFile)
	if err != nil {
		return fail(err)
	}
	if err := arms.Select(splitCSV(o.arms)); err != nil {
		return fail(err)
	}
	launch, err := launcher(o.server, o.serverCmd)
	if err != nil {
		return fail(err)
	}
	cfg = multiturn.Config{Corpus: corpus, Arms: arms, Launch: launch, MaxTurns: o.maxTurns, Parallel: o.parallel}

	plan, err := modelPlan(o.backend, o.models)
	if err != nil {
		return fail(err)
	}

	if o.recordProfile != "" {
		if o.baseline != "" {
			return fail(fmt.Errorf("--record-profile and --baseline do not mix: a recording run is not a measurement"))
		}
		p, err := cassette.LoadProfile(o.recordProfile)
		if err != nil {
			return fail(err)
		}
		if _, err := p.Token(); err != nil {
			return fail(err)
		}
		if st, err := os.Stat(o.recordDir); o.recordDir == "" || err != nil || !st.IsDir() {
			return fail(fmt.Errorf("--record-profile needs --record-dir naming an existing directory"))
		}
		cfg.Record, cfg.RecordDir = p, o.recordDir
	} else if err := corpus.CheckCassettes(); err != nil {
		return fail(err)
	}

	if o.out == "" {
		o.out = fmt.Sprintf("eval/results/%s-multiturn.jsonl", o.server)
	}
	if sameFile(o.out, o.baseline) {
		return fail(fmt.Errorf("--out %s is the same file as --baseline: the run would overwrite the baseline before comparing", o.out))
	}
	f, err := os.OpenFile(o.out, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fail(fmt.Errorf("output %s is not writable: %w", o.out, err))
	}
	_ = f.Close()

	var base *multiturn.Baseline
	if o.baseline != "" {
		bf, err := os.Open(o.baseline)
		if err != nil {
			return fail(err)
		}
		base, err = multiturn.LoadBaseline(bf)
		_ = bf.Close()
		if err != nil {
			return fail(err)
		}
		if err := base.CheckModelIDs(plan); err != nil {
			return fail(err)
		}
		var labels, armNames, taskIDs []string
		for l := range plan {
			labels = append(labels, l)
		}
		for _, a := range arms.Arms {
			armNames = append(armNames, a.Name)
		}
		for _, t := range corpus.Tasks {
			taskIDs = append(taskIDs, t.ID)
		}
		if err := base.CheckOverlap(labels, armNames, taskIDs); err != nil {
			return fail(err)
		}
	}
	return cfg, plan, base, nil
}

// modelPlan maps each label to the wire model id it will record.
func modelPlan(backend, models string) (map[string]string, error) {
	switch backend {
	case "script":
		return map[string]string{"script": "script"}, nil
	case "api", "cli":
		if backend == "api" && os.Getenv("ANTHROPIC_API_KEY") == "" {
			return nil, fmt.Errorf("--backend api needs ANTHROPIC_API_KEY")
		}
		plan := map[string]string{}
		labels := splitCSV(models)
		if len(labels) == 0 {
			return nil, fmt.Errorf("no models given")
		}
		for _, l := range labels {
			if _, dup := plan[l]; dup {
				return nil, fmt.Errorf("duplicate model label %q in --models", l)
			}
			plan[l] = multiturn.ModelID(l)
		}
		return plan, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (script, api, cli)", backend)
	}
}

func buildAgents(backend string, plan map[string]string) ([]multiturn.Agent, error) {
	if backend == "script" {
		return []multiturn.Agent{multiturn.ScriptAgent{}}, nil
	}
	var agents []multiturn.Agent
	if backend == "cli" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		for _, l := range sortedLabels(plan) {
			agents = append(agents, multiturn.NewCLIAgent(l, plan[l], []string{self, "--bridge"}))
		}
		return agents, nil
	}
	for _, l := range sortedLabels(plan) {
		a, err := multiturn.NewAPIAgent(l, plan[l])
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, nil
}

func sortedLabels(plan map[string]string) []string {
	// Small model first, then frontier, then anything ad hoc: the report
	// reads left to right from cheapest.
	rank := func(l string) int {
		if r, ok := map[string]int{"haiku": 0, "sonnet": 1, "opus": 2}[l]; ok {
			return r
		}
		return 99
	}
	var labels []string
	for l := range plan {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		if rank(labels[i]) != rank(labels[j]) {
			return rank(labels[i]) < rank(labels[j])
		}
		return labels[i] < labels[j]
	})
	return labels
}

// stdioProfile is how to spawn one product's stdio server against a replay
// base URL. The server gets a minimal environment — never the caller's — so a
// real credential or config in the operator's shell cannot reach it: HOME is
// a fresh temp dir, the API base is the Player (or recorder), and the token is
// a dummy the Player ignores and the recorder replaces.
type stdioProfile struct {
	bin        string
	args       []string
	baseURLEnv string
	tokenEnv   string
}

var stdioProfiles = map[string]stdioProfile{
	"basecamp": {bin: "basecamp-mcp", args: []string{"stdio"}, baseURLEnv: "BASECAMP_BASE_URL", tokenEnv: "BASECAMP_TOKEN"},
}

func launcher(server, serverCmd string) (multiturn.Launcher, error) {
	if server == "fake" {
		if serverCmd != "" {
			return nil, fmt.Errorf("--server fake runs in process; --server-cmd does not apply")
		}
		return multiturn.ConnectFake, nil
	}
	prof, ok := stdioProfiles[server]
	if !ok {
		return nil, fmt.Errorf("unknown server %q (fake, basecamp)", server)
	}
	var argv []string
	switch {
	case serverCmd != "":
		argv = strings.Fields(serverCmd)
	case os.Getenv("EVAL_"+strings.ToUpper(server)+"_CMD") != "":
		argv = strings.Fields(os.Getenv("EVAL_" + strings.ToUpper(server) + "_CMD"))
	default:
		path, err := exec.LookPath(prof.bin)
		if err != nil {
			return nil, fmt.Errorf("server %q: %s not on PATH (set --server-cmd or EVAL_%s_CMD)", server, prof.bin, strings.ToUpper(server))
		}
		argv = append([]string{path}, prof.args...)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty server command")
	}
	return func(ctx context.Context, arm multiturn.Arm, baseURL string) (*mcp.ClientSession, func(), error) {
		home, err := os.MkdirTemp("", "eval-multiturn-home-")
		if err != nil {
			return nil, nil, err
		}
		args := append(append([]string(nil), argv[1:]...), arm.ServerArgs...)
		cmd := exec.Command(argv[0], args...)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"TMPDIR=" + os.TempDir(),
			prof.baseURLEnv + "=" + baseURL,
			prof.tokenEnv + "=eval-replay-dummy-token",
		}
		for k, v := range arm.ServerEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if os.Getenv("EVAL_DEBUG") != "" {
			cmd.Stderr = os.Stderr
		} else {
			cmd.Stderr = io.Discard
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "eval-multiturn", Version: "0.0.0"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			_ = os.RemoveAll(home)
			return nil, nil, fmt.Errorf("spawn %s: %w", strings.Join(append([]string{argv[0]}, args...), " "), err)
		}
		return session, func() { _ = session.Close(); _ = os.RemoveAll(home) }, nil
	}, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ia, ib)
}
