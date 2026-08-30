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
		baseline  = flag.String("baseline", "", "compare this run against a prior results JSONL; exit nonzero on a score drop, newly-failing scenario, or safety regression")
	)
	flag.Parse()

	ctx := context.Background()

	session, cleanup, err := connect(ctx, *server, *serverCmd)
	if err != nil {
		return err
	}
	defer cleanup()

	var scenarios []eval.Scenario
	if *scenPath != "" {
		data, err := os.ReadFile(*scenPath)
		if err != nil {
			return err
		}
		if scenarios, err = eval.UnmarshalScenarios(data); err != nil {
			return err
		}
	}

	models, err := buildModels(ctx, *backend, *modelsCSV, scenarios, session, eval.GenerateOptions{N: *n, Seed: *seed})
	if err != nil {
		return err
	}

	rep, err := eval.Run(ctx, session, eval.Config{
		Models:    models,
		Scenarios: scenarios,
		Gen:       eval.GenerateOptions{N: *n, Seed: *seed},
	})
	if err != nil {
		return err
	}

	if *writeScen != "" {
		data, err := eval.MarshalScenarios(*server, eval.GenerateOptions{N: *n, Seed: *seed}, rep.Scenarios)
		if err != nil {
			return err
		}
		if err := os.WriteFile(*writeScen, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d scenarios to %s\n", len(rep.Scenarios), *writeScen)
	}

	outPath := *out
	if outPath == "" {
		outPath = fmt.Sprintf("eval/results/%s-v0.jsonl", *server)
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

	if *baseline != "" {
		bf, err := os.Open(*baseline)
		if err != nil {
			return err
		}
		base, err := eval.LoadBaseline(bf)
		_ = bf.Close()
		if err != nil {
			return err
		}
		cmp := eval.CompareToBaseline(base, rep.Records)
		fmt.Print(cmp.Render(*baseline))
		if cmp.HasRegression() {
			return errRegression
		}
	}
	return nil
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

// buildModels resolves the backend and model labels into Model backends. The
// oracle backend needs the scenario corpus, so it generates one from the live
// spec when none was loaded.
func buildModels(ctx context.Context, backend, modelsCSV string, scenarios []eval.Scenario, session *mcp.ClientSession, gen eval.GenerateOptions) ([]eval.Model, error) {
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
		case "oracle":
			if scenarios == nil {
				specs, err := eval.SpecFromSession(ctx, session)
				if err != nil {
					return nil, err
				}
				scenarios = eval.Generate(specs, gen)
			}
			models = append(models, eval.NewOracleModel(scenarios))
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
