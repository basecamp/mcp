// Package multiturn is the eval's multi-turn mode: an agent loop that drives a
// real MCP server as a real client — initialize, tools/list, tools/call, the
// results fed back — for up to N turns per task, against a product backend
// replayed from cassettes, and grades the whole trace by rule.
//
// Where the single-turn loop (package eval) asks "does the model pick the right
// {tool, action, params} from the catalog?", this mode asks "does the agent
// finish a realistic request?" — and, run across arms (bare, +instructions,
// +guide tool, +skill), whether the guidance a server offers helps it finish
// more often, in fewer calls, with fewer wrong ids and no unsafe writes.
//
// Grading reads two records of the episode: the agent's tool calls (op +
// params, surface-independent: a gateway call's action and a flat tool's name
// normalize to the same op) and the replayed backend's exchange log — the
// writes that actually reached "Basecamp", with the bodies the server sent.
// Final-state assertions ride on the second, so an agent that says it posted a
// comment but posted it to the wrong recording fails.
package multiturn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/basecamp/mcp/eval/cassette"
)

// Corpus is a task file: the tasks for one server plus what they share.
type Corpus struct {
	// Server names the server the tasks were written for; a corpus is refused
	// against a different one.
	Server string `json:"server"`
	// Today pins the date the agent is told, so "overdue" and "this week"
	// mean the same thing on every run as the cassettes' fixed dates.
	Today string `json:"today"`
	// CassetteDir resolves task cassette names, relative to the corpus file.
	CassetteDir string `json:"cassette_dir"`
	// MaxTurns is the default per-task turn budget.
	MaxTurns int    `json:"max_turns"`
	Tasks    []Task `json:"tasks"`

	dir string
}

// Task is one realistic request and the rules its trace must satisfy.
type Task struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	Tags   []string `json:"tags,omitempty"`
	// Cassettes are layered in order (later overrides earlier), by name
	// without extension, from the corpus cassette dir.
	Cassettes []string `json:"cassettes"`
	// MaxTurns overrides the corpus default.
	MaxTurns int `json:"max_turns,omitempty"`
	// ReadOnly marks a request that asks for no change: any write attempted
	// against the backend is a safety violation.
	ReadOnly bool `json:"read_only,omitempty"`

	Expect Expect `json:"expect"`
	Reject Reject `json:"reject,omitempty"`

	// Script is the gold path the deterministic script backend plays, and
	// ScriptAnswer its final reply. It proves the cassettes cover a correct
	// solution and turns the loop in CI without a model.
	Script       []ScriptCall `json:"script"`
	ScriptAnswer string       `json:"script_answer"`

	// The corpus-level settings the task runs under, filled at load, so the
	// digest covers what actually governs the episode: the effective turn
	// budget, the pinned date, and the contents of its cassettes.
	effTurns       int
	today          string
	cassetteDigest string
}

// Expect lists what a passing trace must contain. Each pattern is a Go regexp
// (add (?i) for case-insensitive) and must match at least one line.
type Expect struct {
	// Calls match call lines: `<op> <params JSON, keys sorted>`.
	Calls []string `json:"calls,omitempty"`
	// Writes match the backend writes that landed (answered with a 2xx):
	// `METHOD /path?query <body JSON>`.
	Writes []string `json:"writes,omitempty"`
	// WriteGroups are sets of patterns that must all match one and the same
	// landed write — "a to-do assigned to Annie, due Friday, titled X" is one
	// write, not three writes that each get one field right.
	WriteGroups [][]string `json:"write_groups,omitempty"`
	// Answer patterns match the agent's final reply.
	Answer []string `json:"answer,omitempty"`
}

// Reject lists what a trace must never contain. Any match is a safety
// violation — a permanent delete where trash was asked for, a write in the
// wrong place — and fails the task regardless of what else it got right.
// Write patterns match every write attempted, landed or not.
type Reject struct {
	Calls  []string `json:"calls,omitempty"`
	Writes []string `json:"writes,omitempty"`
}

// ScriptCall is one gold tool call.
type ScriptCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

var taskIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// LoadCorpus reads and validates a task file.
func LoadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Corpus
	if err := cassette.DecodeStrict(data, &c); err != nil {
		return nil, fmt.Errorf("tasks %s: %w", path, err)
	}
	c.dir = filepath.Dir(path)
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("tasks %s: %w", path, err)
	}
	for i := range c.Tasks {
		c.Tasks[i].effTurns, c.Tasks[i].today = c.turns(c.Tasks[i]), c.Today
		h := sha256.New()
		for _, name := range c.Tasks[i].Cassettes {
			data, err := os.ReadFile(c.CassettePath(name))
			if err != nil {
				data = []byte("absent") // a recording run creates it
			}
			fmt.Fprintf(h, "%s\x00%x\x00", name, sha256.Sum256(data))
		}
		c.Tasks[i].cassetteDigest = hex.EncodeToString(h.Sum(nil)[:8])
	}
	return &c, nil
}

// CassettePath resolves a cassette name to its file.
func (c *Corpus) CassettePath(name string) string {
	return filepath.Join(c.dir, c.CassetteDir, name+".json")
}

// OverrideTurns applies a run-wide turn budget (--max-turns) to every task,
// digests included: a baseline run under another budget is another task.
func (c *Corpus) OverrideTurns(n int) {
	if n <= 0 {
		return
	}
	for i := range c.Tasks {
		c.Tasks[i].effTurns = n
	}
}

// Filter narrows the corpus to the named task ids, refusing unknown ones.
func (c *Corpus) Filter(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	byID := map[string]Task{}
	for _, t := range c.Tasks {
		byID[t.ID] = t
	}
	var kept []Task
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("task %q selected twice", id)
		}
		seen[id] = true
		t, ok := byID[id]
		if !ok {
			return fmt.Errorf("no task %q in the corpus", id)
		}
		kept = append(kept, t)
	}
	c.Tasks = kept
	return nil
}

func (c *Corpus) validate() error {
	if strings.TrimSpace(c.Server) == "" {
		return fmt.Errorf("corpus names no server")
	}
	if len(c.Tasks) == 0 {
		return fmt.Errorf("corpus has no tasks: a run over nothing must not read as green")
	}
	if c.MaxTurns <= 0 {
		return fmt.Errorf("corpus max_turns must be positive")
	}
	seen := map[string]bool{}
	seenPrompt := map[string]string{}
	for _, t := range c.Tasks {
		if !taskIDRE.MatchString(t.ID) {
			return fmt.Errorf("task id %q must be a lowercase slug (it names the task's recorded cassette file)", t.ID)
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("task %s has an empty prompt", t.ID)
		}
		if prev, dup := seenPrompt[t.Prompt]; dup {
			return fmt.Errorf("tasks %s and %s share a prompt", prev, t.ID)
		}
		seenPrompt[t.Prompt] = t.ID
		if t.MaxTurns < 0 {
			return fmt.Errorf("task %s: max_turns may not be negative", t.ID)
		}
		if len(t.Cassettes) == 0 {
			return fmt.Errorf("task %s names no cassettes", t.ID)
		}
		if len(t.Expect.Calls)+len(t.Expect.Writes)+len(t.Expect.WriteGroups)+len(t.Expect.Answer) == 0 {
			return fmt.Errorf("task %s expects nothing: every trace would pass", t.ID)
		}
		if t.ReadOnly && len(t.Expect.Writes)+len(t.Expect.WriteGroups) > 0 {
			return fmt.Errorf("task %s is read_only but expects writes", t.ID)
		}
		for _, g := range t.Expect.WriteGroups {
			if len(g) == 0 {
				return fmt.Errorf("task %s has an empty write group", t.ID)
			}
		}
		if !t.ReadOnly && len(t.Expect.Writes)+len(t.Expect.WriteGroups) == 0 {
			return fmt.Errorf("task %s asks for a change but asserts no write: final state is the grade", t.ID)
		}
		groups := [][]string{t.Expect.Calls, t.Expect.Writes, t.Expect.Answer, t.Reject.Calls, t.Reject.Writes}
		groups = append(groups, t.Expect.WriteGroups...)
		for _, group := range groups {
			for _, p := range group {
				re, err := regexp.Compile(p)
				if err != nil {
					return fmt.Errorf("task %s: pattern %q: %w", t.ID, p, err)
				}
				// A pattern that matches nothing at all matches everything:
				// an empty answer would satisfy it, a vacuous check.
				if re.MatchString("") {
					return fmt.Errorf("task %s: pattern %q matches the empty string, so it checks nothing", t.ID, p)
				}
			}
		}
		if len(t.Script) == 0 {
			return fmt.Errorf("task %s has no gold script: the corpus must prove each task is solvable against its cassettes", t.ID)
		}
		for i, s := range t.Script {
			if s.Tool == "" {
				return fmt.Errorf("task %s: script step %d names no tool", t.ID, i+1)
			}
		}
	}
	return nil
}

// Digest identifies what a task asks and how it is graded — prompt,
// cassettes, effective turn budget, pinned date, read-only flag,
// expectations, rejections — so a
// baseline cell is only compared with a run of the same task definition.
// The gold script is left out: it proves the corpus, it does not grade.
func (t Task) Digest() string {
	data, _ := json.Marshal(struct {
		Prompt    string
		Cassettes []string
		MaxTurns  int
		Today     string
		Data      string
		ReadOnly  bool
		Expect    Expect
		Reject    Reject
	}{t.Prompt, t.Cassettes, t.effTurns, t.today, t.cassetteDigest, t.ReadOnly, t.Expect, t.Reject})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// CheckCassettes proves every task's cassettes exist and load — checked
// before a replay run spends anything. (A recording run skips it: it is what
// creates them.)
func (c *Corpus) CheckCassettes() error {
	for _, t := range c.Tasks {
		var layers []*cassette.Cassette
		for _, name := range t.Cassettes {
			cs, err := cassette.Load(c.CassettePath(name))
			if err != nil {
				return fmt.Errorf("task %s: %w", t.ID, err)
			}
			layers = append(layers, cs)
		}
		if err := cassette.ValidateLayers(layers...); err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
	}
	return nil
}

// turns returns the task's turn budget.
func (c *Corpus) turns(t Task) int {
	if t.MaxTurns > 0 {
		return t.MaxTurns
	}
	return c.MaxTurns
}
