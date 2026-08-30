package eval

import (
	"strings"
	"testing"
)

// records builds a small run: each triple is (scenarioID, score, safetyOK).
func records(model string, triples ...any) []Record {
	var out []Record
	for i := 0; i < len(triples); i += 3 {
		out = append(out, Record{
			Model:               model,
			ScenarioID:          triples[i].(string),
			Score:               triples[i+1].(float64),
			AnnotationRespected: triples[i+2].(bool),
		})
	}
	return out
}

func baselineFrom(t *testing.T, recs []Record) *Baseline {
	t.Helper()
	var b strings.Builder
	if err := WriteJSONL(&b, recs); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	base, err := LoadBaseline(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	return base
}

func TestBaselineIdenticalRunHasNoRegression(t *testing.T) {
	recs := records("haiku", "a.x", 1.0, true, "b.y", 1.0, true)
	base := baselineFrom(t, recs)
	cmp := CompareToBaseline(base, recs)
	if cmp.HasRegression() {
		t.Fatalf("identical run must not regress: %+v", cmp.Regressions)
	}
	if len(cmp.Added) != 0 || len(cmp.Removed) != 0 {
		t.Fatalf("identical run must have no added/removed: %+v", cmp)
	}
}

func TestBaselineFlagsNewlyFailing(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 1.0, true, "b.y", 1.0, true))
	now := records("haiku", "a.x", 1.0, true, "b.y", 0.0, true) // b.y regressed
	cmp := CompareToBaseline(base, now)
	if !cmp.HasRegression() || len(cmp.Regressions) != 1 {
		t.Fatalf("want one regression, got %+v", cmp.Regressions)
	}
	if r := cmp.Regressions[0]; r.Kind != KindNewlyFailing || r.ScenarioID != "b.y" {
		t.Fatalf("want newly-failing b.y, got %+v", r)
	}
}

func TestBaselineFlagsSafetyRegressionEvenAtEqualScore(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 0.0, true))
	now := records("haiku", "a.x", 0.0, false) // same score, safety now violated
	cmp := CompareToBaseline(base, now)
	if len(cmp.Regressions) != 1 || cmp.Regressions[0].Kind != KindSafety {
		t.Fatalf("want a safety regression, got %+v", cmp.Regressions)
	}
}

func TestBaselineFlagsScoreDropBelowPass(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 0.75, true))
	now := records("haiku", "a.x", 0.5, true)
	cmp := CompareToBaseline(base, now)
	if len(cmp.Regressions) != 1 || cmp.Regressions[0].Kind != KindScoreDrop {
		t.Fatalf("want a score-drop, got %+v", cmp.Regressions)
	}
}

func TestBaselineFlagsDimensionRegressionAtEqualFailingScore(t *testing.T) {
	// Both runs fail (score 0), but the baseline got the action right and now
	// gets it wrong — strictly worse, and invisible to a score-only compare.
	prev := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: true, ActionMatch: true, ParamsMatch: false, AnnotationRespected: true}}
	base := baselineFrom(t, prev)
	now := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: true, ActionMatch: false, ParamsMatch: false, AnnotationRespected: true}}
	cmp := CompareToBaseline(base, now)
	if len(cmp.Regressions) != 1 || cmp.Regressions[0].Kind != KindDimension {
		t.Fatalf("want a dimension regression, got %+v", cmp.Regressions)
	}
	if cmp.Regressions[0].Detail != "action_match" {
		t.Fatalf("want action_match detail, got %q", cmp.Regressions[0].Detail)
	}
}

func TestBaselineNoDimensionRegressionWhenBothFailSameWay(t *testing.T) {
	rec := Record{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: true, ActionMatch: false, AnnotationRespected: true}
	base := baselineFrom(t, []Record{rec})
	cmp := CompareToBaseline(base, []Record{rec})
	if cmp.HasRegression() {
		t.Fatalf("identical failing cell must not gate: %+v", cmp.Regressions)
	}
}

func TestLoadBaselineRejectsEmpty(t *testing.T) {
	if _, err := LoadBaseline(strings.NewReader("")); err == nil {
		t.Fatalf("an empty baseline must be rejected, not silently disable the gate")
	}
	if _, err := LoadBaseline(strings.NewReader("\n  \n")); err == nil {
		t.Fatalf("a blank-only baseline must be rejected")
	}
}

func TestBaselineReportsDimensionImprovementWithoutGating(t *testing.T) {
	prev := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: false, ActionMatch: false, AnnotationRespected: true}}
	base := baselineFrom(t, prev)
	now := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: true, ActionMatch: false, AnnotationRespected: true}}
	cmp := CompareToBaseline(base, now)
	if cmp.HasRegression() {
		t.Fatalf("a dimension improvement must not gate: %+v", cmp.Regressions)
	}
	if len(cmp.Improved) != 1 || cmp.Improved[0].Detail != "tool_match" {
		t.Fatalf("want a reported tool_match improvement, got %+v", cmp.Improved)
	}
}

func TestBaselineImprovementNeverGates(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 0.0, true))
	now := records("haiku", "a.x", 1.0, true)
	cmp := CompareToBaseline(base, now)
	if cmp.HasRegression() {
		t.Fatalf("an improvement must not gate: %+v", cmp.Regressions)
	}
	if len(cmp.Improved) != 1 {
		t.Fatalf("want one improved cell, got %+v", cmp.Improved)
	}
}

func TestBaselineAddedAndRemovedAreReportedNotGated(t *testing.T) {
	base := baselineFrom(t, records("haiku", "gone.z", 1.0, true, "keep.k", 1.0, true))
	now := records("haiku", "keep.k", 1.0, true, "fresh.f", 1.0, true)
	cmp := CompareToBaseline(base, now)
	if cmp.HasRegression() {
		t.Fatalf("corpus edits must not gate: %+v", cmp.Regressions)
	}
	if len(cmp.Added) != 1 || cmp.Added[0] != "haiku/fresh.f" {
		t.Fatalf("want fresh.f added, got %+v", cmp.Added)
	}
	if len(cmp.Removed) != 1 || cmp.Removed[0] != "haiku/gone.z" {
		t.Fatalf("want gone.z removed, got %+v", cmp.Removed)
	}
}

func TestBaselineComparesPerModel(t *testing.T) {
	// The same scenario under a different model is a different cell, so a new
	// model's failure is "added", not a regression of the baseline model.
	base := baselineFrom(t, records("haiku", "a.x", 1.0, true))
	now := append(records("haiku", "a.x", 1.0, true), records("sonnet", "a.x", 0.0, true)...)
	cmp := CompareToBaseline(base, now)
	if cmp.HasRegression() {
		t.Fatalf("a new model is not a regression: %+v", cmp.Regressions)
	}
	if len(cmp.Added) != 1 || cmp.Added[0] != "sonnet/a.x" {
		t.Fatalf("want sonnet/a.x added, got %+v", cmp.Added)
	}
}
