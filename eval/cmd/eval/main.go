// Command eval runs the structural MCP eval loop against a gateway server and
// prints a scored scenario×model table plus a measured cost figure.
//
// It connects to the server one of two ways: an in-process fake catalog
// (--server fake), or a real product server spawned over stdio
// (--server fizzy, or any --server-cmd). Either way it reads the server's own
// tool listing and describe payloads as the spec — no product backend or
// credentials are touched, since the eval only lists and describes.
//
// Usage:
//
//	go run ./eval/cmd/eval --server fake --backend oracle --n 12
//	go run ./eval/cmd/eval --server fizzy --server-cmd "/path/fizzy-mcp stdio --writes" \
//	    --backend cli --models haiku --n 12
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/eval"
)

// errRegression is returned when a --baseline comparison finds a worse cell, so
// the process exits nonzero — the merge-blocking signal for CI.
var errRegression = errors.New("baseline regression detected")

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "eval: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		server    = flag.String("server", "fake", "server to eval: fake (in-process) or a product name like fizzy")
		serverCmd = flag.String("server-cmd", "", "command to spawn a stdio MCP server (overrides --server mapping)")
		modelsCSV = flag.String("models", "haiku", "comma-separated model labels (haiku, sonnet)")
		backend   = flag.String("backend", "cli", "model backend: cli, api, or oracle (deterministic, no spend)")
		n         = flag.Int("n", 12, "number of scenarios")
		seed      = flag.Int64("seed", 1, "generation seed (reproducible)")
		out       = flag.String("out", "", "JSONL results path (default eval/results/<server>-v0.jsonl)")
		scenPath  = flag.String("scenarios", "", "load scenarios from this JSON instead of generating")
		writeScen = flag.String("write-scenarios", "", "write the generated corpus to this JSON")
		requireP  = flag.Bool("require-pass", false, "exit nonzero unless every record passes (for the deterministic oracle smoke)")
		baseline  = flag.String("baseline", "", "compare this run against a prior results JSONL; exit nonzero on a score drop, newly-failing scenario, or safety regression")
	)
	flag.Parse()

	ctx := context.Background()

	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("eval/results/%s-v0.jsonl", *server)
	}

	// One gate for every check that needs neither a server connection nor a
	// paid model call: flags, backend, API key, a non-empty model set, output
	// writability and aliasing, corpus loading and server match, and — against
	// a baseline — model-id identity and (for a loaded corpus) cell overlap.
	// A bad flag, a missing key, or a mismatched baseline fails here, never
	// after authenticating a live server (basecamp) or billing a run.
	base, scenarios, gen, plan, err := preflight(preflightOpts{
		server: *server, backend: *backend, models: *modelsCSV,
		gen: eval.GenerateOptions{N: *n, Seed: *seed},
		out: outPath, writeScen: *writeScen,
		scenPath: *scenPath, baseline: *baseline,
	})
	if err != nil {
		return err
	}

	session, cleanup, err := connect(ctx, *server, *serverCmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// A generated corpus's scenario ids are not known until the live catalog
	// is read, so its baseline overlap is the one check that must wait for the
	// connection — still before any model call. A loaded corpus was already
	// overlap-checked in preflight.
	if scenarios == nil {
		specs, err := eval.SpecFromSession(ctx, session)
		if err != nil {
			return err
		}
		scenarios = eval.Generate(specs, gen)
		if base != nil {
			if err := base.CheckOverlap(planLabels(plan), scenarioIDs(scenarios)); err != nil {
				return err
			}
		}
	}

	models, err := buildModels(*backend, *modelsCSV, scenarios)
	if err != nil {
		return err
	}

	rep, err := eval.Run(ctx, session, eval.Config{
		Models:    models,
		Scenarios: scenarios,
		Gen:       gen,
	})
	if err != nil {
		return err
	}

	// A generated corpus can come up short of --n: the catalog has fewer
	// distinct actions, or colliding framings were dropped. Say so, or a
	// smaller experiment than asked for reads as the one that was requested.
	if *scenPath == "" && len(rep.Scenarios) < gen.N {
		fmt.Fprintf(os.Stderr, "note: generated %d of the %d scenarios requested (fewer distinct actions in the catalog, or colliding framings dropped)\n", len(rep.Scenarios), gen.N)
	}

	if *writeScen != "" {
		data, err := eval.MarshalScenarios(*server, gen, rep.Scenarios)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*writeScen, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d scenarios to %s\n", len(rep.Scenarios), *writeScen)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	if err := eval.WriteJSONL(f, rep.Records); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	fmt.Print(rep.Render(*server))
	fmt.Fprintf(os.Stderr, "\nwrote %d records to %s\n", len(rep.Records), outPath)

	if base != nil {
		cmp, err := eval.CompareToBaseline(base, rep.Records)
		if err != nil {
			return err
		}
		fmt.Print(cmp.Render(*baseline))
		if cmp.HasRegression() {
			return errRegression
		}
	}

	if *requireP {
		if err := eval.RequirePass(rep.Records); err != nil {
			return fmt.Errorf("--require-pass: %w", err)
		}
	}
	return nil
}

// preflightWritable proves path can be opened for writing — creating it empty
// when absent, never truncating what is already there — so an unwritable
// destination fails before any spend rather than after.
func preflightWritable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("output %s is not writable: %w", path, err)
	}
	return f.Close()
}

// checkAliases refuses an output path that names a file the run also reads or
// writes for another purpose, where the write would silently destroy it:
// --out truncates (os.Create), so it must not be the source corpus or the
// corpus just written; --write-scenarios replaces its file, so it must not be
// the baseline (loaded earlier, so the comparison would still run and report
// nothing about the results it just overwrote). --out over --baseline is the
// documented compare-then-overwrite flow and stays allowed, as does rewriting
// a loaded corpus in place. Identity comparison, so symlink aliases count.
func checkAliases(out, writeScen, scenPath, baseline string) error {
	pairs := []struct{ aFlag, a, bFlag, b string }{
		{"--out", out, "--scenarios", scenPath},
		{"--out", out, "--write-scenarios", writeScen},
		{"--write-scenarios", writeScen, "--baseline", baseline},
	}
	for _, p := range pairs {
		if sameFile(p.a, p.b) {
			return fmt.Errorf("%s %s is the same file as %s %s", p.aFlag, p.a, p.bFlag, p.b)
		}
	}
	return nil
}

// sameFile reports whether two paths name one file, following symlinks, so an
// alias is caught as well as a literal repeat. A path that does not exist
// names nothing and so aliases nothing.
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

// connect returns a client session to the chosen server plus a cleanup func.
func connect(ctx context.Context, server, serverCmd string) (*mcp.ClientSession, func(), error) {
	if server == "fake" && serverCmd == "" {
		srv, err := eval.NewFakeServer()
		if err != nil {
			return nil, nil, err
		}
		return eval.ConnectInProcess(ctx, srv)
	}

	if prof, ok := serverProfiles[server]; ok && !prof.hermetic {
		fmt.Fprintf(os.Stderr, "eval: %s is not hermetic — its stdio server authenticates at startup, so this run needs real credentials and network (a --live target)\n", server)
	}

	fields, err := serverFields(server, serverCmd)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(fields[0], fields[1:]...)
	cmd.Env = childEnv(server)
	if os.Getenv("EVAL_DEBUG") == "" {
		cmd.Stderr = io.Discard
	} else {
		cmd.Stderr = os.Stderr
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "eval-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("spawn %q: %w", strings.Join(fields, " "), err)
	}
	return session, func() { _ = session.Close() }, nil
}

// splitCommand tokenizes a command line, honoring single and double quotes so a
// path or argument containing spaces survives intact. It rejects an empty
// command rather than indexing into no fields.
func splitCommand(s string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inField := false
	var quote rune // 0, '\'' or '"'
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inField = true
		case r == ' ' || r == '\t' || r == '\n':
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			cur.WriteRune(r)
			inField = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quote in command %q", s)
	}
	if inField {
		fields = append(fields, cur.String())
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty server command")
	}
	return fields, nil
}

// serverProfile describes how to launch and prime one product's stdio MCP
// server. The eval package is product-agnostic — it reads whatever a session
// lists and describes — so everything product-specific lives here in the
// command glue, never in eval/. Adding a product is one map entry, which is
// what "2nd & 3rd server, no per-server code" means in practice.
type serverProfile struct {
	bin  string            // default binary name, resolved on PATH
	args []string          // stdio subcommand and flags
	env  map[string]string // env injected only when absent (dummy startup creds)
	// hermetic is true when tools/list + describe need no live backend or real
	// credentials, so the eval runs offline at zero cost. A non-hermetic server
	// is a credentialed / --live target: it is wired here so it composes the
	// moment it can run, but a default (uncredentialed) spawn will fail.
	hermetic bool
}

// serverProfiles is the known-product registry. fizzy and hey serve their
// catalog's list+describe surface from the vendored SDK model without touching
// a backend, so a dummy token (fizzy) or no token (hey) starts them
// hermetically. basecamp-mcp's stdio authenticates eagerly — it fetches
// authorization.json before serving — so it needs real credentials and network
// until the cassette player (hillclimb #2) can stub its startup.
var serverProfiles = map[string]serverProfile{
	"fizzy":    {bin: "fizzy-mcp", args: []string{"stdio", "--writes"}, env: map[string]string{"FIZZY_TOKEN": "eval-structural-only"}, hermetic: true},
	"hey":      {bin: "hey-mcp", args: []string{"stdio"}, hermetic: true},
	"basecamp": {bin: "basecamp-mcp", args: []string{"stdio"}, hermetic: false},
}

// serverFields resolves the argv to spawn for a server. An explicit
// --server-cmd or an EVAL_<PRODUCT>_CMD is a user-supplied command line, so it
// is tokenized by splitCommand (honoring quotes). The registry default, by
// contrast, returns the PATH-resolved binary and its args as an argv slice
// directly — never round-tripping through a joined string — so a binary
// installed under a directory whose name contains spaces still spawns
// correctly.
func serverFields(server, serverCmd string) ([]string, error) {
	if serverCmd != "" {
		return splitCommand(serverCmd)
	}
	if v := os.Getenv("EVAL_" + strings.ToUpper(server) + "_CMD"); v != "" {
		return splitCommand(v)
	}
	prof, ok := serverProfiles[server]
	if !ok {
		return nil, fmt.Errorf("no --server-cmd given and no default for server %q (set --server-cmd or EVAL_%s_CMD)", server, strings.ToUpper(server))
	}
	path, err := exec.LookPath(prof.bin)
	if err != nil {
		return nil, fmt.Errorf("server %q: %s not found on PATH (set --server-cmd or EVAL_%s_CMD): %w", server, prof.bin, strings.ToUpper(server), err)
	}
	return append([]string{path}, prof.args...), nil
}

// childEnv supplies the spawned server its environment plus any dummy startup
// credential the registry injects, so a hermetic server starts without real
// secrets. Existing env always wins — a real token is never overwritten.
func childEnv(server string) []string {
	env := os.Environ()
	for k, v := range serverProfiles[server].env {
		if os.Getenv(k) == "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// loadCorpus reads a cached corpus and checks it belongs to the selected
// server, without needing a session: a corpus grades product-specific
// tool/action names, so running it against a different live catalog would
// silently mislabel the mismatch as model failures. (Corpora written before
// the server metadata existed carry an empty server and can't be checked.)
// It returns the corpus's own generation metadata so a rewrite keeps it.
func loadCorpus(path, server string) ([]eval.Scenario, eval.GenerateOptions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, eval.GenerateOptions{}, err
	}
	corpus, err := eval.LoadCorpus(data)
	if err != nil {
		return nil, eval.GenerateOptions{}, fmt.Errorf("%s: %w", path, err)
	}
	if corpus.Server != "" && corpus.Server != server {
		return nil, eval.GenerateOptions{}, fmt.Errorf("corpus %s was generated for server %q but --server is %q", path, corpus.Server, server)
	}
	return corpus.Scenarios, eval.GenerateOptions{N: corpus.N, Seed: corpus.Seed}, nil
}

// preflightOpts carries the parsed flags the preflight gate reads.
type preflightOpts struct {
	server, backend, models, out, writeScen, scenPath, baseline string
	gen                                                         eval.GenerateOptions
}

// preflight runs every check that needs neither a server connection nor a paid
// model call, returning the loaded baseline (nil when --baseline is unset), the
// loaded corpus (nil when the run will generate one), the generation options (a
// loaded corpus keeps its own), and the label->wire-model-id plan the run will
// produce. It is the single gate the command runs before connect: the only
// checks left for afterward are the ones that need the live catalog — reading
// the spec to generate a corpus, and the cell overlap for a generated corpus,
// whose ids do not exist until then.
func preflight(o preflightOpts) (*eval.Baseline, []eval.Scenario, eval.GenerateOptions, map[string]string, error) {
	gen := o.gen

	// Backend, model labels, and the API key when the API backend is selected
	// — all resolvable from flags alone.
	plan, err := modelPlan(o.backend, o.models)
	if err != nil {
		return nil, nil, gen, nil, err
	}

	// Output destinations must be writable and must not alias a corpus or the
	// baseline (os.Create truncates).
	for _, path := range []string{o.out, o.writeScen} {
		if path != "" {
			if err := preflightWritable(path); err != nil {
				return nil, nil, gen, nil, err
			}
		}
	}
	if err := checkAliases(o.out, o.writeScen, o.scenPath, o.baseline); err != nil {
		return nil, nil, gen, nil, err
	}

	// Baseline file: present, non-empty, well-formed.
	var base *eval.Baseline
	if o.baseline != "" {
		bf, err := os.Open(o.baseline)
		if err != nil {
			return nil, nil, gen, nil, err
		}
		base, err = eval.LoadBaseline(bf)
		_ = bf.Close()
		if err != nil {
			return nil, nil, gen, nil, err
		}
	}

	// Loaded corpus: readable, well-formed, and generated for this server.
	var scenarios []eval.Scenario
	if o.scenPath != "" {
		if scenarios, gen, err = loadCorpus(o.scenPath, o.server); err != nil {
			return nil, nil, gen, nil, err
		}
	}

	// Against a baseline: each label the run will use must name the same wire
	// model the baseline recorded under it, and — when the corpus is already
	// known — the run must share a cell with the baseline. (A generated
	// corpus's overlap is checked after connect, once its ids exist.)
	if base != nil {
		if err := base.CheckModelIDs(plan); err != nil {
			return nil, nil, gen, nil, err
		}
		if scenarios != nil {
			if err := base.CheckOverlap(planLabels(plan), scenarioIDs(scenarios)); err != nil {
				return nil, nil, gen, nil, err
			}
		}
	}
	return base, scenarios, gen, plan, nil
}

// modelPlan maps each requested label to the wire model id the backend will
// record for it, validating the backend name, the API key when the API backend
// is selected, and that at least one label was given — all without a session or
// a paid call, so these deterministic errors surface before connect. The oracle
// backend answers under a single "oracle" label regardless of --models.
func modelPlan(backend, modelsCSV string) (map[string]string, error) {
	var labels []string
	for _, l := range strings.Split(modelsCSV, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("no models given")
	}
	plan := map[string]string{}
	switch backend {
	case "cli":
		for _, l := range labels {
			plan[l] = cliModelID(l)
		}
	case "api":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return nil, fmt.Errorf("--backend api needs ANTHROPIC_API_KEY")
		}
		for _, l := range labels {
			plan[l] = apiModelID(l)
		}
	case "oracle":
		plan["oracle"] = "oracle"
	default:
		return nil, fmt.Errorf("unknown backend %q (cli, api, oracle)", backend)
	}
	return plan, nil
}

// planLabels returns the run's model labels; scenarioIDs the corpus's ids.
func planLabels(plan map[string]string) []string {
	labels := make([]string, 0, len(plan))
	for l := range plan {
		labels = append(labels, l)
	}
	return labels
}

func scenarioIDs(scenarios []eval.Scenario) []string {
	ids := make([]string, 0, len(scenarios))
	for _, sc := range scenarios {
		ids = append(ids, sc.ID)
	}
	return ids
}

// buildModels resolves the backend and model labels into Model backends. The
// oracle answers from the resolved corpus.
func buildModels(backend, modelsCSV string, scenarios []eval.Scenario) ([]eval.Model, error) {
	// The oracle answers from the corpus under a single "oracle" label, so it
	// is built once regardless of --models. One instance per label would
	// collide on that shared label and Run would reject the whole run as
	// duplicate — so a valid multi-label oracle invocation always failed.
	if backend == "oracle" {
		return []eval.Model{eval.NewOracleModel(scenarios)}, nil
	}
	labels := strings.Split(modelsCSV, ",")
	var models []eval.Model
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		switch backend {
		case "cli":
			models = append(models, eval.NewCLIModel(label, cliModelID(label)))
		case "api":
			m, err := eval.NewAPIModel(label, apiModelID(label))
			if err != nil {
				return nil, err
			}
			models = append(models, m)
		default:
			return nil, fmt.Errorf("unknown backend %q (cli, api, oracle)", backend)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no models given")
	}
	return models, nil
}

// cliModelID maps a label to a `claude --model` alias.
func cliModelID(label string) string {
	switch label {
	case "haiku":
		return "haiku"
	case "sonnet":
		return "sonnet"
	default:
		return label
	}
}

// apiModelID maps a label to an Anthropic API model id.
func apiModelID(label string) string {
	switch label {
	case "haiku":
		return "claude-3-5-haiku-latest"
	case "sonnet":
		return "claude-sonnet-4-5"
	default:
		return label
	}
}
