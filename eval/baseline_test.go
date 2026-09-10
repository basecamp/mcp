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
	dup := jsonl(t, append(records("haiku", "a.x", 1.0, true), records("haiku", "a.x", 0.0, true)...))
	_, err := LoadBaseline(strings.NewReader(dup))
	if err == nil {
		t.Fatal("duplicate (model, scenario_id) accepted")
	}
	if !strings.Contains(err.Error(), "haiku/a.x") {
		t.Fatalf("error does not name the duplicated cell: %v", err)
	}

	// Same scenario under a different model is a distinct cell, not a dup.
	ok := jsonl(t, append(records("haiku", "a.x", 1.0, true), records("sonnet", "a.x", 1.0, true)...))
	if _, err := LoadBaseline(strings.NewReader(ok)); err != nil {
		t.Fatalf("distinct models rejected as duplicates: %v", err)
	}
}

// jsonl renders records the way the CLI writes them.
func jsonl(t *testing.T, recs []Record) string {
	t.Helper()
	var b strings.Builder
	if err := WriteJSONL(&b, recs); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestLoadBaselineRejectsRecordsMissingFields pins the last silent-disarm
// shape: a line with identity but without the grading fields decodes to an
// all-zero cell, so every current cell compares as unchanged or improved. The
// check is the whole Record shape — any always-written field absent is a
// rejection — not one field at a time.
func TestLoadBaselineRejectsRecordsMissingFields(t *testing.T) {
	cases := []string{
		`{"model":"haiku","scenario_id":"x"}`,
		`{"model":"haiku","scenario_id":"x","score":1}`,
		`{"model":"haiku","scenario_id":"x","score":1,"tool_match":true,"action_match":true,"params_match":true}`,
	}
	for _, line := range cases {
		_, err := LoadBaseline(strings.NewReader(line + "\n"))
		if err == nil {
			t.Fatalf("baseline record without grading fields accepted: %s", line)
		}
		if !strings.Contains(err.Error(), "missing") {
			t.Fatalf("error does not say what is missing: %v", err)
		}
	}
	// What the CLI writes loads, optional fields (error, pricing_estimated)
	// absent or present.
	full := jsonl(t, []Record{
		{Model: "haiku", ScenarioID: "x", Score: 1, AnnotationRespected: true},
		{Model: "haiku", ScenarioID: "y", Error: "boom", PricingEstimated: true},
	})
	if _, err := LoadBaseline(strings.NewReader(full)); err != nil {
		t.Fatalf("a CLI-written baseline was rejected: %v", err)
	}
}

// TestRenderShowsImprovementOnlyComparison pins that a run whose only
// differences are improvements renders each improved cell with its scenario
// and detail, and never claims that every cell held its baseline score.
func TestRenderShowsImprovementOnlyComparison(t *testing.T) {
	prev := []Record{
		{Model: "haiku", ScenarioID: "a.x", Score: 0, AnnotationRespected: true},
		{Model: "haiku", ScenarioID: "b.y", Score: 0, ToolMatch: false, AnnotationRespected: true},
	}
	now := []Record{
		{Model: "haiku", ScenarioID: "a.x", Score: 1, AnnotationRespected: true},
		{Model: "haiku", ScenarioID: "b.y", Score: 0, ToolMatch: true, AnnotationRespected: true},
	}
	cmp := compare(t, baselineFrom(t, prev), now)
	if cmp.HasRegression() || len(cmp.Improved) != 2 {
		t.Fatalf("want two improvements and no regression, got %+v", cmp)
	}
	out := cmp.Render("prior.jsonl")
	if strings.Contains(out, "no change") {
		t.Fatalf("improvement-only comparison rendered as no change:\n%s", out)
	}
	if !strings.Contains(out, "improved   haiku") || !strings.Contains(out, "a.x") || !strings.Contains(out, "0.00 -> 1.00") {
		t.Fatalf("score improvement row missing:\n%s", out)
	}
	if !strings.Contains(out, "b.y") || !strings.Contains(out, "tool_match false -> true") {
		t.Fatalf("dimension improvement row missing:\n%s", out)
	}
	// A genuinely unchanged run still says so.
	same := compare(t, baselineFrom(t, prev), prev)
	if !strings.Contains(same.Render("prior.jsonl"), "no change") {
		t.Fatalf("unchanged run not rendered as no change:\n%s", same.Render("prior.jsonl"))
	}
}

// TestBaselineCheckOverlapBeforeSpend pins the pre-run half of the
// zero-overlap rule: a --models label the baseline lacks, or a disjoint
// corpus, is refused before any model call rather than by the comparison
// after the run has been paid for.
func TestBaselineCheckOverlapBeforeSpend(t *testing.T) {
	base := baselineFrom(t, records("haiku", "a.x", 1.0, true, "b.y", 1.0, true))
	if err := base.CheckOverlap([]string{"sonnet"}, []string{"a.x", "b.y"}); err == nil {
		t.Fatal("disjoint model label accepted")
	}
	if err := base.CheckOverlap([]string{"haiku"}, []string{"c.z"}); err == nil {
		t.Fatal("disjoint corpus accepted")
	}
	if err := base.CheckOverlap(nil, []string{"a.x"}); err == nil {
		t.Fatal("no models accepted")
	}
	if err := base.CheckOverlap([]string{"sonnet", "haiku"}, []string{"c.z", "a.x"}); err != nil {
		t.Fatalf("one overlapping cell must be enough: %v", err)
	}
}

// TestCompareRejectsModelIDMismatch pins that a label shared by two different
// wire models is not a like-for-like cell: the comparison refuses, naming
// both ids, instead of gating a model swap as a routing regression. Records
// without a model_id (written before it existed) are not checked.
func TestCompareRejectsModelIDMismatch(t *testing.T) {
	prev := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 1, AnnotationRespected: true, ModelID: "claude-3-5-haiku-latest"}}
	base := baselineFrom(t, prev)
	now := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 0, AnnotationRespected: true, ModelID: "haiku"}}
	_, err := CompareToBaseline(base, now)
	if err == nil {
		t.Fatal("different wire models under one label compared as one cell")
	}
	if !strings.Contains(err.Error(), "claude-3-5-haiku-latest") || !strings.Contains(err.Error(), `"haiku"`) {
		t.Fatalf("error does not name both models: %v", err)
	}
	same := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 1, AnnotationRespected: true, ModelID: "claude-3-5-haiku-latest"}}
	if _, err := CompareToBaseline(base, same); err != nil {
		t.Fatalf("same wire model rejected: %v", err)
	}
	legacy := []Record{{Model: "haiku", ScenarioID: "a.x", Score: 1, AnnotationRespected: true}}
	if _, err := CompareToBaseline(baselineFrom(t, legacy), now); err != nil {
		t.Fatalf("legacy baseline without model_id must still compare: %v", err)
	}
}

// TestRenderImprovementRowsNameTheModel pins that every rendered category
// carries the model: two models improving one scenario must be two
// distinguishable rows.
func TestRenderImprovementRowsNameTheModel(t *testing.T) {
	prev := append(records("haiku", "a.x", 0.0, true), records("sonnet", "a.x", 0.0, true)...)
	now := append(records("haiku", "a.x", 1.0, true), records("sonnet", "a.x", 1.0, true)...)
	out := compare(t, baselineFrom(t, prev), now).Render("prior.jsonl")
	if !strings.Contains(out, "improved   haiku ") || !strings.Contains(out, "improved   sonnet ") {
		t.Fatalf("improvement rows do not name the model:\n%s", out)
	}
}

// TestBaselineCheckModelIDsBeforeSpend pins the pre-run half of the
// like-for-like rule: a label whose baseline cells were produced by a
// different wire model is refused before any model call, not by the
// comparison after the run has been paid for.
func TestBaselineCheckModelIDsBeforeSpend(t *testing.T) {
	base := baselineFrom(t, []Record{
		{Model: "haiku", ScenarioID: "a.x", Score: 1, AnnotationRespected: true, ModelID: "claude-3-5-haiku-latest"},
		{Model: "legacy", ScenarioID: "a.x", Score: 1, AnnotationRespected: true},
	})
	if err := base.CheckModelIDs(map[string]string{"haiku": "haiku"}); err == nil {
		t.Fatal("different wire model under the baseline's label accepted")
	}
	if err := base.CheckModelIDs(map[string]string{"haiku": "claude-3-5-haiku-latest"}); err != nil {
		t.Fatalf("same wire model rejected: %v", err)
	}
	if err := base.CheckModelIDs(map[string]string{"legacy": "anything", "sonnet": "claude-sonnet-4-5"}); err != nil {
		t.Fatalf("legacy (no model_id) or absent labels must not be checked: %v", err)
	}
}
