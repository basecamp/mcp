package multiturn

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The regression gate mirrors the single-turn one (package eval, baseline.go)
// with the cell widened to (model, arm, task): the same guards against a gate
// that compares nothing (no overlap, duplicate cells, records missing fields,
// a label now naming a different model), and the same split between gating
// changes (newly-failing, score drop, new safety violation, an errored
// episode that measured nothing) and reported ones
// (improved, added, removed). Efficiency — calls, tokens, cost — is reported
// by the summary table and never gates: a model that takes one more call to
// finish is not a regression of the server.

var recordKeys = func() []string {
	data, err := json.Marshal(Record{})
	if err != nil {
		panic(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		panic(err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}()

func cellKey(model, arm, task string) string { return model + "\x00" + arm + "\x00" + task }

// Baseline is a prior run indexed by cell.
type Baseline struct {
	cells map[string]Record
}

// LoadBaseline reads a prior run's JSONL.
func LoadBaseline(r io.Reader) (*Baseline, error) {
	b := &Baseline{cells: map[string]Record{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			return nil, fmt.Errorf("decode baseline record: %w", err)
		}
		for _, k := range recordKeys {
			v, ok := fields[k]
			if !ok {
				return nil, fmt.Errorf("baseline record missing %q: %s", k, truncate(line, 200))
			}
			// null decodes to the zero value without complaint; a baseline
			// "pass":null would read as a failing cell and hide a regression.
			if string(v) == "null" {
				return nil, fmt.Errorf("baseline record has null %q: %s", k, truncate(line, 200))
			}
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("decode baseline record: %w", err)
		}
		if rec.Server == "" || rec.Backend == "" || rec.Model == "" || rec.ModelID == "" || rec.Arm == "" || rec.TaskID == "" || rec.TaskDigest == "" || rec.ArmDigest == "" {
			// model_id included: without it the like-for-like model check
			// would silently skip the cell.
			return nil, fmt.Errorf("baseline record missing server, backend, model, model_id, arm, task_id, task_digest, or arm_digest: %s", truncate(line, 200))
		}
		k := cellKey(rec.Model, rec.Arm, rec.TaskID)
		if _, dup := b.cells[k]; dup {
			return nil, fmt.Errorf("baseline has duplicate cell %s/%s/%s: split the runs into separate files", rec.Model, rec.Arm, rec.TaskID)
		}
		b.cells[k] = rec
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(b.cells) == 0 {
		return nil, fmt.Errorf("baseline has no records: it cannot provide a regression signal")
	}
	return b, nil
}

// CheckModelIDs refuses, before spend, a label the baseline recorded under a
// different wire model.
func (b *Baseline) CheckModelIDs(plan map[string]string) error {
	for _, rec := range b.cells {
		if want, ok := plan[rec.Model]; ok && rec.ModelID != "" && want != rec.ModelID {
			return fmt.Errorf("label %q: baseline was produced by model %q, this run would use %q — not the same model; regenerate the baseline or run under a distinct label", rec.Model, rec.ModelID, want)
		}
	}
	return nil
}

// CheckExperiment refuses, before spend, a baseline from another server or
// agent backend: an API run and a CLI run, or two products sharing a task
// id, are not like-for-like cells.
func (b *Baseline) CheckExperiment(server, backend string) error {
	for _, rec := range b.cells {
		if rec.Server != server || rec.Backend != backend {
			return fmt.Errorf("baseline is %s via %s, this run would be %s via %s — not the same experiment", rec.Server, rec.Backend, server, backend)
		}
	}
	return nil
}

// CheckDigests refuses, before spend, a baseline graded under a different
// definition of a task this run will grade.
func (b *Baseline) CheckDigests(digests map[string]string) error {
	for _, rec := range b.cells {
		if d, ok := digests[rec.TaskID]; ok && d != rec.TaskDigest {
			return fmt.Errorf("task %s changed since the baseline (prompt, cassettes, or grading): regenerate the baseline or rename the task", rec.TaskID)
		}
	}
	return nil
}

// CheckArmDigests refuses, before spend, a baseline taken under a different
// definition of an arm this run will use.
func (b *Baseline) CheckArmDigests(digests map[string]string) error {
	for _, rec := range b.cells {
		if d, ok := digests[rec.Arm]; ok && d != rec.ArmDigest {
			return fmt.Errorf("arm %s changed since the baseline (switches, server args, guide tools, or skill): regenerate the baseline or rename the arm", rec.Arm)
		}
	}
	return nil
}

// CheckOverlap refuses, before spend, a run that shares no cell with the
// baseline.
func (b *Baseline) CheckOverlap(models, arms, tasks []string) error {
	for _, m := range models {
		for _, a := range arms {
			for _, t := range tasks {
				if _, ok := b.cells[cellKey(m, a, t)]; ok {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("baseline shares no (model, arm, task) cell with this run: nothing would be compared")
}

// Change is one cell that moved against the baseline.
type Change struct {
	Model, Arm, TaskID string
	Kind               string
	OldScore, NewScore float64
}

// Comparison is a run diffed against a baseline.
type Comparison struct {
	Regressions []Change
	Improved    []Change
	Added       []string
	Removed     []string
}

// HasRegression reports whether any cell got worse.
func (c Comparison) HasRegression() bool { return len(c.Regressions) > 0 }

// Compare diffs records against the baseline, cell by cell.
func Compare(base *Baseline, records []Record) (Comparison, error) {
	var cmp Comparison
	matched := 0
	seen := map[string]bool{}
	for _, r := range records {
		k := cellKey(r.Model, r.Arm, r.TaskID)
		seen[k] = true
		prev, ok := base.cells[k]
		if !ok {
			cmp.Added = append(cmp.Added, r.Model+"/"+r.Arm+"/"+r.TaskID)
			continue
		}
		matched++
		if prev.ArmDigest != r.ArmDigest {
			return cmp, fmt.Errorf("arm %s changed since the baseline (switches, server args, guide tools, or skill): regenerate the baseline or rename the arm", r.Arm)
		}
		if prev.TaskDigest != r.TaskDigest {
			return cmp, fmt.Errorf("task %s changed since the baseline (prompt, cassettes, or grading): regenerate the baseline or rename the task", r.TaskID)
		}
		if prev.Server != r.Server || prev.Backend != r.Backend {
			return cmp, fmt.Errorf("cell %s/%s/%s: baseline is %s via %s, this run %s via %s — not the same experiment", r.Model, r.Arm, r.TaskID, prev.Server, prev.Backend, r.Server, r.Backend)
		}
		if prev.ModelID != "" && r.ModelID != "" && prev.ModelID != r.ModelID {
			return cmp, fmt.Errorf("cell %s/%s/%s: baseline model %q, this run %q — not like-for-like", r.Model, r.Arm, r.TaskID, prev.ModelID, r.ModelID)
		}
		ch := Change{Model: r.Model, Arm: r.Arm, TaskID: r.TaskID, OldScore: prev.Score, NewScore: r.Score}
		switch {
		case r.Error != "":
			// An episode that errored was not measured; whatever the baseline
			// held, the gate cannot say it held.
			ch.Kind = "error"
			cmp.Regressions = append(cmp.Regressions, ch)
		case prev.Safety == 0 && r.Safety > 0:
			ch.Kind = "safety"
			cmp.Regressions = append(cmp.Regressions, ch)
		case prev.Pass && !r.Pass:
			ch.Kind = "newly-failing"
			cmp.Regressions = append(cmp.Regressions, ch)
		case r.Score < prev.Score:
			ch.Kind = "score-drop"
			cmp.Regressions = append(cmp.Regressions, ch)
		case (!prev.Pass && r.Pass) || r.Score > prev.Score:
			ch.Kind = "improved"
			cmp.Improved = append(cmp.Improved, ch)
		}
	}
	for k, prev := range base.cells {
		if !seen[k] {
			cmp.Removed = append(cmp.Removed, prev.Model+"/"+prev.Arm+"/"+prev.TaskID)
		}
	}
	less := func(s []Change) func(i, j int) bool {
		return func(i, j int) bool {
			return s[i].Model+s[i].Arm+s[i].TaskID < s[j].Model+s[j].Arm+s[j].TaskID
		}
	}
	sort.Slice(cmp.Regressions, less(cmp.Regressions))
	sort.Slice(cmp.Improved, less(cmp.Improved))
	sort.Strings(cmp.Added)
	sort.Strings(cmp.Removed)
	if matched == 0 {
		return cmp, fmt.Errorf("baseline comparison matched no cells")
	}
	return cmp, nil
}

// Render prints the comparison.
func (c Comparison) Render(path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nBASELINE COMPARE — vs %s\n", path)
	if !c.HasRegression() && len(c.Improved)+len(c.Added)+len(c.Removed) == 0 {
		b.WriteString("no change: every cell holds its baseline result\n")
		return b.String()
	}
	if c.HasRegression() {
		fmt.Fprintf(&b, "\nREGRESSIONS (%d):\n", len(c.Regressions))
		for _, r := range c.Regressions {
			fmt.Fprintf(&b, "  %-14s %s/%s/%s  %.2f -> %.2f\n", r.Kind, r.Model, r.Arm, r.TaskID, r.OldScore, r.NewScore)
		}
	}
	for _, r := range c.Improved {
		fmt.Fprintf(&b, "improved   %s/%s/%s  %.2f -> %.2f\n", r.Model, r.Arm, r.TaskID, r.OldScore, r.NewScore)
	}
	for _, s := range c.Added {
		fmt.Fprintf(&b, "added      %s\n", s)
	}
	for _, s := range c.Removed {
		fmt.Fprintf(&b, "removed    %s\n", s)
	}
	return b.String()
}
