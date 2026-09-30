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
	"bytes"
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
}

// Expect lists what a passing trace must contain. Each pattern is a Go regexp
// (add (?i) for case-insensitive) and must match at least one line.
type Expect struct {
	// Calls match call lines: `<op> <params JSON, keys sorted>`.
	Calls []string `json:"calls,omitempty"`
	// Writes match the backend writes that landed (answered, status < 400):
	// `METHOD /path?query <body JSON>`.
	Writes []string `json:"writes,omitempty"`
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

// LoadCorpus reads and validates a task file.
func LoadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Corpus
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("tasks %s: %w", path, err)
	}
	c.dir = filepath.Dir(path)
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("tasks %s: %w", path, err)
	}
	return &c, nil
}

// CassettePath resolves a cassette name to its file.
func (c *Corpus) CassettePath(name string) string {
	return filepath.Join(c.dir, c.CassetteDir, name+".json")
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
	for _, id := range ids {
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
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("a task has no id")
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
		if len(t.Cassettes) == 0 {
			return fmt.Errorf("task %s names no cassettes", t.ID)
		}
		if len(t.Expect.Calls)+len(t.Expect.Writes)+len(t.Expect.Answer) == 0 {
			return fmt.Errorf("task %s expects nothing: every trace would pass", t.ID)
		}
		if t.ReadOnly && len(t.Expect.Writes) > 0 {
			return fmt.Errorf("task %s is read_only but expects writes", t.ID)
		}
		if !t.ReadOnly && len(t.Expect.Writes) == 0 {
			return fmt.Errorf("task %s asks for a change but asserts no write: final state is the grade", t.ID)
		}
		for _, group := range [][]string{t.Expect.Calls, t.Expect.Writes, t.Expect.Answer, t.Reject.Calls, t.Reject.Writes} {
			for _, p := range group {
				if _, err := regexp.Compile(p); err != nil {
					return fmt.Errorf("task %s: pattern %q: %w", t.ID, p, err)
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

// CheckCassettes proves every task's cassettes exist and load — checked
// before a replay run spends anything. (A recording run skips it: it is what
// creates them.)
func (c *Corpus) CheckCassettes() error {
	for _, t := range c.Tasks {
		for _, name := range t.Cassettes {
			if _, err := cassette.Load(c.CassettePath(name)); err != nil {
				return fmt.Errorf("task %s: %w", t.ID, err)
			}
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
