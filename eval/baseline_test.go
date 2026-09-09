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

// compare runs a comparison that is expected to match cells.
func compare(t *testing.T, base *Baseline, recs []Record) Comparison {
	t.Helper()
	cmp, err := CompareToBaseline(base, recs)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	return cmp
}

func TestBaselineIdenticalRunHasNoRegression(t *testing.T) {
	recs := records("haiku", "a.x", 1.0, true, "b.y", 1.0, true)
	base := baselineFrom(t, recs)
	cmp := compare(t, base, recs)
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
	cmp := compare(t, base, now)
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
	cmp := compare(t, base, now)
	if len(cmp.Regressions) != 1 || cmp.Regressions[0].Kind != KindSafety {
		t.Fatalf("want a safety regression, got %+v", cmp.Regressions)
	}
}

func TestBaselineFlagsScoreDropBelowPass(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 0.75, true))
	now := records("haiku", "a.x", 0.5, true)
	cmp := compare(t, base, now)
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
	cmp := compare(t, base, now)
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
	cmp := compare(t, base, []Record{rec})
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

func TestLoadBaselineRejectsRecordsWithoutIdentity(t *testing.T) {
	// A syntactically valid but identity-less line ({}) must not slip past the
	// empty guard by inserting a blank-key cell and disarming the gate.
	if _, err := LoadBaseline(strings.NewReader("{}\n")); err == nil {
		t.Fatalf("a record without model/scenario_id must be rejected")
	}
	if _, err := LoadBaseline(strings.NewReader(`{"model":"haiku"}` + "\n")); err == nil {
		t.Fatalf("a record missing scenario_id must be rejected")
	}
}

func TestBaselineReportsDimensionImprovementWithoutGating(t *testing.T) {
	prev := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: false, ActionMatch: false, AnnotationRespected: true}}
	base := baselineFrom(t, prev)
	now := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, ToolMatch: true, ActionMatch: false, AnnotationRespected: true}}
	cmp := compare(t, base, now)
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
	cmp := compare(t, base, now)
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
	cmp := compare(t, base, now)
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
	cmp := compare(t, base, now)
	if cmp.HasRegression() {
		t.Fatalf("a new model is not a regression: %+v", cmp.Regressions)
	}
	if len(cmp.Added) != 1 || cmp.Added[0] != "sonnet/a.x" {
		t.Fatalf("want sonnet/a.x added, got %+v", cmp.Added)
	}
}

// TestCompareRejectsNoMatchingCells pins the fail-closed rule for a comparison
// with zero (model, scenario_id) overlap. Added and removed cells are each
// individually non-gating, so without this a mislabelled --models run or an
// emptied corpus would report "no regression" having compared nothing.
func TestCompareRejectsNoMatchingCells(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 1.0, true, "b.y", 1.0, true))

	// Same scenarios, different model label: every cell added, every baseline
	// cell removed, nothing compared.
	cmp, err := CompareToBaseline(base, records("sonnet", "a.x", 1.0, true, "b.y", 1.0, true))
	if err == nil {
		t.Fatalf("zero-overlap comparison accepted: %+v", cmp)
	}
	if cmp.HasRegression() {
		t.Fatal("zero-overlap comparison must fail on the error, not by inventing regressions")
	}

	// Same model, disjoint scenario ids.
	if _, err := CompareToBaseline(base, records("haiku", "c.z", 1.0, true)); err == nil {
		t.Fatal("comparison with disjoint scenario ids accepted")
	}

	// No records at all.
	if _, err := CompareToBaseline(base, nil); err == nil {
		t.Fatal("comparison of an empty run accepted")
	}

	// A single overlapping cell is enough to compare, even alongside
	// added and removed ones.
	if _, err := CompareToBaseline(base, records("haiku", "a.x", 1.0, true, "c.z", 1.0, true)); err != nil {
		t.Fatalf("partial overlap rejected: %v", err)
	}
}

// TestLoadBaselineRejectsDuplicateCells pins the other silent-disarm path: two
// runs concatenated into one append-only file put the same cell in twice, and
// keeping the last lets a failing run overwrite a passing one so the same
// failure now compares equal and clears the gate.
func TestLoadBaselineRejectsDuplicateCells(t *testing.T) {
	dup := `{"model":"haiku","scenario_id":"a.x","score":1}` + "\n" +
		`{"model":"haiku","scenario_id":"a.x","score":0}` + "\n"
	_, err := LoadBaseline(strings.NewReader(dup))
	if err == nil {
		t.Fatal("duplicate (model, scenario_id) accepted")
	}
	if !strings.Contains(err.Error(), "haiku/a.x") {
		t.Fatalf("error does not name the duplicated cell: %v", err)
	}

	// Same scenario under a different model is a distinct cell, not a dup.
	ok := `{"model":"haiku","scenario_id":"a.x","score":1}` + "\n" +
		`{"model":"sonnet","scenario_id":"a.x","score":1}` + "\n"
	if _, err := LoadBaseline(strings.NewReader(ok)); err != nil {
		t.Fatalf("distinct models rejected as duplicates: %v", err)
	}
}
