package multiturn

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/eval/cassette"
)

// Launcher starts the server under test for one arm, pointed at a backend
// base URL, and returns a connected client session and its cleanup.
type Launcher func(ctx context.Context, arm Arm, baseURL string) (*mcp.ClientSession, func(), error)

// Config parameterizes a multi-turn run.
type Config struct {
	Corpus *Corpus
	Arms   *ArmSet
	Agents []Agent
	Launch Launcher
	// MaxTurns, when positive, overrides every task's turn budget.
	MaxTurns int
	// Record, when set, runs against the live test account through a
	// recording proxy instead of replaying, and writes each task's cassette
	// (merged with any already there) to RecordDir.
	Record    *cassette.Profile
	RecordDir string
	// Progress, when set, receives one line per finished episode.
	Progress io.Writer
	// Parallel is how many episodes run at once (default 1). Recording
	// forces 1: episodes merge into shared cassette files.
	Parallel int
}

// Report is a finished run.
type Report struct {
	Server  string
	Tasks   []Task
	Arms    []Arm
	Records []Record
}

// Run drives every agent over every arm and task and grades each episode.
// Every arm is realized against the server once before any agent runs, so an
// arm the server cannot honor fails the run before the first paid turn.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if len(cfg.Agents) == 0 {
		return nil, fmt.Errorf("no agents to run")
	}
	seen := map[string]bool{}
	for _, a := range cfg.Agents {
		if seen[a.Label()] {
			return nil, fmt.Errorf("duplicate model label %q", a.Label())
		}
		seen[a.Label()] = true
	}
	if cfg.Corpus.Server != cfg.Arms.Server {
		return nil, fmt.Errorf("tasks are for server %q but arms are for %q", cfg.Corpus.Server, cfg.Arms.Server)
	}
	if cfg.Record != nil {
		// Every recorded episode performs its writes on the live account,
		// so a second task, agent or arm would record the state the first
		// left. One episode, then reseed.
		if len(cfg.Agents) != 1 || len(cfg.Arms.Arms) != 1 || len(cfg.Corpus.Tasks) != 1 {
			return nil, fmt.Errorf("recording runs one task, one agent, one arm (got %d, %d, %d): each episode mutates the live test account, so every recording starts from a freshly seeded one", len(cfg.Corpus.Tasks), len(cfg.Agents), len(cfg.Arms.Arms))
		}
		cfg.Parallel = 1
	} else if err := cfg.Corpus.CheckCassettes(); err != nil {
		return nil, err
	}
	if err := preflightArms(ctx, cfg); err != nil {
		return nil, err
	}

	rep := &Report{Server: cfg.Corpus.Server, Tasks: cfg.Corpus.Tasks, Arms: cfg.Arms.Arms}
	type job struct {
		i     int
		agent Agent
		arm   Arm
		task  Task
	}
	var jobs []job
	for _, agent := range cfg.Agents {
		for _, arm := range cfg.Arms.Arms {
			for _, task := range cfg.Corpus.Tasks {
				jobs = append(jobs, job{len(jobs), agent, arm, task})
			}
		}
	}
	// Episodes are independent — each has its own backend and server
	// process — so they may run concurrently; records keep job order.
	rep.Records = make([]Record, len(jobs))
	workers := cfg.Parallel
	if workers < 1 {
		workers = 1
	}
	queue := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				rec := runEpisode(ctx, cfg, j.agent, j.arm, j.task)
				rec.Server, rec.Backend = cfg.Corpus.Server, backendOf(j.agent)
				rep.Records[j.i] = rec
				if cfg.Progress != nil {
					status := "PASS"
					if !rec.Pass {
						status = "FAIL"
					}
					if rec.Error != "" {
						status = "ERR"
					}
					mu.Lock()
					fmt.Fprintf(cfg.Progress, "%-8s %-14s %-28s %s calls=%d turns=%d $%.4f\n", j.agent.Label(), j.arm.Name, j.task.ID, status, rec.Calls, rec.Turns, rec.CostUSD)
					mu.Unlock()
				}
			}
		}()
	}
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	wg.Wait()
	// A recording run exists to write cassettes; one it failed to write
	// fails the run, whatever the episodes scored.
	for _, r := range rep.Records {
		if strings.HasPrefix(r.Error, recordErrPrefix) {
			return rep, fmt.Errorf("%s/%s/%s: %s: %s", r.Model, r.Arm, r.TaskID, r.Error, r.detail)
		}
	}
	return rep, nil
}

// preflightArms realizes each arm against a server replaying the first task's
// cassettes (a server may, like basecamp-mcp, fetch its identity before it
// serves): the surface (instructions, guide tools, skill) comes from the
// server's catalog, so any task's cassettes prove it.
func preflightArms(ctx context.Context, cfg Config) error {
	if cfg.Record != nil {
		// A recording realizes its one arm in its one episode: a separate
		// preflight session would talk to the live account (a server may
		// write at startup) and discard what it did.
		return nil
	}
	var cs []*cassette.Cassette
	for _, name := range cfg.Corpus.Tasks[0].Cassettes {
		if cfg.Record != nil {
			break
		}
		c, err := cassette.Load(cfg.Corpus.CassettePath(name))
		if err != nil {
			return err
		}
		cs = append(cs, c)
	}
	for _, arm := range cfg.Arms.Arms {
		var be backend
		var url string
		if cfg.Record != nil {
			// Recording: the task cassettes may not exist yet, so the live
			// test account answers the server's startup instead.
			r, err := cassette.NewRecorder(cfg.Record)
			if err != nil {
				return err
			}
			be, url = r, r.Start()
		} else {
			p := cassette.NewPlayer(cs...)
			be, url = p, p.Start()
		}
		session, cleanup, err := cfg.Launch(ctx, arm, url)
		if err != nil {
			be.Close()
			return fmt.Errorf("arm %q: launch: %w", arm.Name, err)
		}
		_, err = cfg.Arms.Realize(ctx, session, arm)
		cleanup()
		be.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// backend is a started player or recorder.
type backend interface {
	Backend
	Close()
}

func runEpisode(ctx context.Context, cfg Config, agent Agent, arm Arm, task Task) Record {
	fail := func(err error) Record {
		r := Record{Model: agent.Label(), ModelID: agent.ModelID(), Arm: arm.Name, TaskID: task.ID, Error: err.Error()}
		if cfg.Record != nil {
			// In a recording run, any failure means no cassette: fatal.
			r.Error, r.detail = recordErrPrefix+"episode could not start, nothing saved", err.Error()
		}
		return r
	}

	var be backend
	var url string
	var rec *cassette.Recorder
	if cfg.Record != nil {
		r, err := cassette.NewRecorder(cfg.Record)
		if err != nil {
			return fail(err)
		}
		rec, be, url = r, r, r.Start()
	} else {
		var cs []*cassette.Cassette
		for _, name := range task.Cassettes {
			c, err := cassette.Load(cfg.Corpus.CassettePath(name))
			if err != nil {
				return fail(err)
			}
			cs = append(cs, c)
		}
		p := cassette.NewPlayer(cs...)
		be, url = p, p.Start()
	}
	defer be.Close()

	session, cleanup, err := cfg.Launch(ctx, arm, url)
	if err != nil {
		return fail(fmt.Errorf("launch: %w", err))
	}
	defer cleanup()

	surf, err := cfg.Arms.Realize(ctx, session, arm)
	if err != nil {
		return fail(err)
	}
	turns := cfg.Corpus.turns(task)
	if cfg.MaxTurns > 0 {
		turns = cfg.MaxTurns
	}
	prompt := surf
	if h, ok := agent.(interface{ HostInjectsInstructions() bool }); ok && h.HostInjectsInstructions() {
		// The host puts the server's instructions in its own system prompt
		// (it receives them in initialize), so the harness must not repeat
		// them.
		cp := *surf
		cp.Instructions = ""
		prompt = &cp
	}
	ep := NewEpisode(task, surf, SystemPrompt(prompt, cfg.Corpus.Today), turns, session, be)
	runErr := agent.Run(ctx, ep)

	r := grade(ep, cfg.Arms.isGuideTool)
	r.Model, r.ModelID, r.Arm = agent.Label(), agent.ModelID(), arm.Name
	r.InTokens, r.OutTokens = ep.Usage.InputTokens, ep.Usage.OutputTokens
	r.CacheWriteTokens, r.CacheReadTokens = ep.Usage.CacheWriteTokens, ep.Usage.CacheReadTokens
	r.CostUSD, r.PricingEstimated = costOf(agent.ModelID(), ep.Usage)
	if ep.hasCLICost {
		r.CostUSD, r.PricingEstimated = ep.cliCost, false
	}
	if runErr != nil {
		r.Error = runErr.Error()
		r.Pass = false
	}

	if rec != nil {
		// Only a whole episode is a recording: one that errored part-way
		// would save (or merge in) a partial cassette.
		// Error keeps a fixed category; the detail (which can echo live or
		// prompt text) goes to the operator through Run's error only.
		if runErr != nil {
			r.Error, r.detail, r.Pass = recordErrPrefix+"episode errored, nothing saved", runErr.Error(), false
		} else if faults := rec.Faults(); len(faults) > 0 {
			// The agent may have recovered, but the recording did not: an
			// answer the recorder refused or a transient upstream failure
			// would replay wrong. Reseed and record again.
			r.Error, r.detail, r.Pass = recordErrPrefix+fmt.Sprintf("recording incomplete, nothing saved (%d fault(s))", len(faults)), strings.Join(faults, "; "), false
		} else if err := saveRecording(cfg, task, rec); err != nil {
			r.Error, r.detail, r.Pass = recordErrPrefix+"cassette save failed", err.Error(), false
		}
		// A recording run's product is the scrubbed cassette. Its results
		// record keeps the scores but drops what carries free text from the
		// live account or the prompt — the trace (arguments and results),
		// the answer, the reasons — so no path out of a recording bypasses
		// the scrubber.
		r.Trace, r.Answer, r.Reasons = nil, "", nil
	}
	return r
}

// backendOf names an agent's backend for the record; an agent that does not
// say is "custom".
func backendOf(a Agent) string {
	if b, ok := a.(interface{ Backend() string }); ok {
		return b.Backend()
	}
	return "custom"
}

const recordErrPrefix = "record: "

// saveRecording merges what one episode recorded into the task's cassette in
// RecordDir.
func saveRecording(cfg Config, task Task, rec *cassette.Recorder) error {
	path := filepath.Join(cfg.RecordDir, task.ID+".json")
	got := rec.Cassette(task.ID, fmt.Sprintf("Recorded from test profile %q for task %s.", cfg.Record.Name, task.ID))
	if err := got.Validate(); err != nil {
		return fmt.Errorf("recorded cassette would not load: %w", err)
	}
	if prev, err := cassette.Load(path); err == nil {
		cassette.Merge(prev, got)
		got = prev
	} else if !os.IsNotExist(err) {
		return err
	}
	// The merged whole must load and replay, not only the new part.
	if err := got.Validate(); err != nil {
		return fmt.Errorf("merged cassette would not load: %w", err)
	}
	if err := cassette.ValidateLayers(got); err != nil {
		return fmt.Errorf("merged cassette would not replay: %w", err)
	}
	return got.Save(path)
}

// SystemPrompt composes the model's system prompt for an arm. The preamble is
// identical across arms, so the only difference an arm makes is the guidance
// it adds.
func SystemPrompt(s *Surface, today string) string {
	var b strings.Builder
	name := s.ServerName
	if name == "" {
		name = "the product"
	}
	fmt.Fprintf(&b, "You are an assistant connected to the user's account through the %s MCP tools. ", name)
	b.WriteString("Complete the user's request by calling the tools. The user is not available to answer questions, so make reasonable choices instead of asking. ")
	b.WriteString("When you are done, reply with a brief final answer for the user.\n")
	if today != "" {
		fmt.Fprintf(&b, "Today's date is %s.\n", today)
	}
	if s.Instructions != "" {
		fmt.Fprintf(&b, "\n# MCP server instructions (%s)\n\n%s\n", name, strings.TrimSpace(s.Instructions))
	}
	if s.Skill != "" {
		fmt.Fprintf(&b, "\n# Skill\n\n%s\n", strings.TrimSpace(s.Skill))
	}
	return b.String()
}
