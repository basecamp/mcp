package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// This file adds the "quickly aware of regressions" half of the loop: a run's
// JSONL records are the append-only store, and a new run is compared against a
// prior one cell-for-cell. A score drop or a newly-failing scenario is a
// regression the caller can gate on (nonzero exit), so a catalog, SDK, or
// prompt change that quietly degrades routing fails a check instead of merging
// unnoticed.
//
// Comparison is keyed on (model, scenario_id): the same model answering the
// same framing is the like-for-like cell. Catalog/SDK/API SHAs (the design's
// three-SHA row) are not yet on the Record, so a corpus regenerated from a
// changed catalog shows up here as added/removed scenarios rather than a
// same-cell drop — surfaced, not silently dropped.

// baselineKey identifies one comparable cell.
func baselineKey(model, scenarioID string) string { return model + "\x00" + scenarioID }

// Baseline is a prior run indexed for cell lookup.
type Baseline struct {
	cells map[string]Record
}

// LoadBaseline reads a prior run's JSONL into a comparable baseline. Blank
// lines are skipped so a hand-edited file still loads.
func LoadBaseline(r io.Reader) (*Baseline, error) {
	b := &Baseline{cells: map[string]Record{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("decode baseline record: %w", err)
		}
		b.cells[baselineKey(rec.Model, rec.ScenarioID)] = rec
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return b, nil
}

// RegressionKind names why a cell regressed.
type RegressionKind string

const (
	// KindNewlyFailing: the cell passed in the baseline and fails now.
	KindNewlyFailing RegressionKind = "newly-failing"
	// KindScoreDrop: the cell's score fell without crossing the pass line
	// (both runs sub-pass, but worse now).
	KindScoreDrop RegressionKind = "score-drop"
	// KindSafety: the cell respected safety in the baseline and violates it
	// now — a read/lookup framing that newly resolves to a destructive action.
	KindSafety RegressionKind = "safety"
)

// Regression is one cell that got worse against the baseline.
type Regression struct {
	Model      string
	ScenarioID string
	Kind       RegressionKind
	OldScore   float64
	NewScore   float64
}

// Comparison is the full diff of a run against a baseline.
type Comparison struct {
	Regressions []Regression // score drops, newly-failing, and safety regressions
	Added       []string     // "model/scenario" present now, absent in baseline
	Removed     []string     // "model/scenario" present in baseline, absent now
	Improved    []Regression // cells that got better (reported, never gated)
}

// HasRegression reports whether any cell got worse — the gate signal.
func (c Comparison) HasRegression() bool { return len(c.Regressions) > 0 }

// CompareToBaseline diffs the current records against the baseline, cell by
// cell. Added scenarios are never regressions (there is nothing to compare);
// removed scenarios are reported so a shrinking corpus is visible but do not
// gate, since dropping a scenario is a corpus edit, not a model regression.
func CompareToBaseline(base *Baseline, records []Record) Comparison {
	var cmp Comparison
	seen := map[string]bool{}
	for _, rec := range records {
		key := baselineKey(rec.Model, rec.ScenarioID)
		seen[key] = true
		prev, ok := base.cells[key]
		if !ok {
			cmp.Added = append(cmp.Added, rec.Model+"/"+rec.ScenarioID)
			continue
		}
		// Safety is the sharpest signal: a newly destructive answer to a
		// read/lookup framing is always a regression, even if the score math
		// would not otherwise flag it.
		if prev.AnnotationRespected && !rec.AnnotationRespected {
			cmp.Regressions = append(cmp.Regressions, Regression{
				Model: rec.Model, ScenarioID: rec.ScenarioID, Kind: KindSafety,
				OldScore: prev.Score, NewScore: rec.Score,
			})
			continue
		}
		switch {
		case prev.Score >= 1 && rec.Score < 1:
			cmp.Regressions = append(cmp.Regressions, Regression{
				Model: rec.Model, ScenarioID: rec.ScenarioID, Kind: KindNewlyFailing,
				OldScore: prev.Score, NewScore: rec.Score,
			})
		case rec.Score < prev.Score:
			cmp.Regressions = append(cmp.Regressions, Regression{
				Model: rec.Model, ScenarioID: rec.ScenarioID, Kind: KindScoreDrop,
				OldScore: prev.Score, NewScore: rec.Score,
			})
		case rec.Score > prev.Score:
			cmp.Improved = append(cmp.Improved, Regression{
				Model: rec.Model, ScenarioID: rec.ScenarioID, Kind: "improved",
				OldScore: prev.Score, NewScore: rec.Score,
			})
		}
	}
	for key, prev := range base.cells {
		if !seen[key] {
			cmp.Removed = append(cmp.Removed, prev.Model+"/"+prev.ScenarioID)
		}
	}
	sort.Slice(cmp.Regressions, func(i, j int) bool { return regLess(cmp.Regressions[i], cmp.Regressions[j]) })
	sort.Slice(cmp.Improved, func(i, j int) bool { return regLess(cmp.Improved[i], cmp.Improved[j]) })
	sort.Strings(cmp.Added)
	sort.Strings(cmp.Removed)
	return cmp
}

func regLess(a, b Regression) bool {
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	return a.ScenarioID < b.ScenarioID
}

// Render renders the comparison as a human- and CI-log-readable block.
func (c Comparison) Render(baselinePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nBASELINE COMPARE — vs %s\n", baselinePath)
	if !c.HasRegression() && len(c.Added) == 0 && len(c.Removed) == 0 {
		fmt.Fprintf(&b, "no change: every cell holds its baseline score")
		if len(c.Improved) > 0 {
			fmt.Fprintf(&b, " (%d improved)", len(c.Improved))
		}
		b.WriteString("\n")
		return b.String()
	}
	if c.HasRegression() {
		fmt.Fprintf(&b, "\nREGRESSIONS (%d):\n", len(c.Regressions))
		fmt.Fprintf(&b, "%-10s  %-14s  %-32s  %s\n", "model", "kind", "scenario", "score")
		for _, r := range c.Regressions {
			fmt.Fprintf(&b, "%-10s  %-14s  %-32s  %.2f -> %.2f\n", r.Model, r.Kind, r.ScenarioID, r.OldScore, r.NewScore)
		}
	}
	for _, s := range c.Improved {
		fmt.Fprintf(&b, "improved   %-32s  %.2f -> %.2f\n", s.ScenarioID, s.OldScore, s.NewScore)
	}
	for _, s := range c.Added {
		fmt.Fprintf(&b, "added      %s\n", s)
	}
	for _, s := range c.Removed {
		fmt.Fprintf(&b, "removed    %s\n", s)
	}
	return b.String()
}
