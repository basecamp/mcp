package eval

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixtureSpecs is a small hand-built catalog spanning the four classes, used by
// the generator and grader tests without a server round-trip.
func fixtureSpecs() []ActionSpec {
	return []ActionSpec{
		{Tool: "t_boards", Action: "get_board", Summary: "Get one board", ReadOnly: true, Idempotent: true,
			Params: []ParamSpec{{Name: "board_id", In: "path", Required: true, Type: "string"}}},
		{Tool: "t_boards", Action: "delete_board", Summary: "Delete a board", Destructive: true,
			Params: []ParamSpec{{Name: "board_id", In: "path", Required: true, Type: "string"}}},
		{Tool: "t_boards", Action: "update_board", Summary: "Update a board", Idempotent: true,
			Params: []ParamSpec{
				{Name: "board_id", In: "path", Required: true, Type: "string"},
				{Name: "name", In: "body", Required: false, Type: "string"}}},
		{Tool: "t_cards", Action: "create_card", Summary: "Create a card",
			Params: []ParamSpec{
				{Name: "board_id", In: "path", Required: true, Type: "string"},
				{Name: "title", In: "body", Required: true, Type: "string"},
				{Name: "status", In: "body", Required: false, Type: "string", Enum: []any{"published", "drafted"}}}},
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	specs := fixtureSpecs()
	a := Generate(specs, GenerateOptions{N: 4, Seed: 42})
	b := Generate(specs, GenerateOptions{N: 4, Seed: 42})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same seed produced different corpora:\n%+v\n%+v", a, b)
	}
	c := Generate(specs, GenerateOptions{N: 4, Seed: 43})
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced identical corpora")
	}
}

func TestGenerateCapsAtDistinctActions(t *testing.T) {
	specs := fixtureSpecs()
	got := Generate(specs, GenerateOptions{N: 100, Seed: 1})
	if len(got) != len(specs) {
		t.Fatalf("want %d scenarios (one per action), got %d", len(specs), len(got))
	}
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s.ID] {
			t.Fatalf("duplicate scenario %q — sampling is not without replacement", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestGoldParamsValidateAgainstSpec(t *testing.T) {
	specs := fixtureSpecs()
	idx := Index(specs)
	for _, sc := range Generate(specs, GenerateOptions{N: 100, Seed: 5}) {
		spec, _ := idx.lookup(sc.GoldTool, sc.GoldAction)
		if ok, reasons := validateParams(spec, sc.GoldParams); !ok {
			t.Fatalf("gold params for %s failed validation: %v", sc.ID, reasons)
		}
		// The gold answer must score a perfect grade — the oracle relies on it.
		gold := Proposal{Tool: sc.GoldTool, Action: sc.GoldAction, Params: sc.GoldParams}
		if r := Grade(sc, gold, idx); !r.Pass() {
			t.Fatalf("gold proposal for %s did not pass: %+v", sc.ID, r)
		}
	}
}

func TestReadScenariosCarryReadOnlyFlag(t *testing.T) {
	for _, sc := range Generate(fixtureSpecs(), GenerateOptions{N: 100, Seed: 9}) {
		if (sc.Class == ClassRead) != sc.ReadOnlyFramed {
			t.Fatalf("scenario %s: class=%s but readOnlyFramed=%v", sc.ID, sc.Class, sc.ReadOnlyFramed)
		}
	}
}

func TestGradeDimensions(t *testing.T) {
	idx := Index(fixtureSpecs())
	readScenario := Scenario{
		ID: "t_boards.get_board", Class: ClassRead, GoldTool: "t_boards", GoldAction: "get_board",
		GoldParams: map[string]any{"board_id": "42"}, ReadOnlyFramed: true,
	}

	cases := []struct {
		name     string
		scenario Scenario
		prop     Proposal
		want     Result
	}{
		{
			name:     "correct",
			scenario: readScenario,
			prop:     Proposal{Tool: "t_boards", Action: "get_board", Params: map[string]any{"board_id": "42"}},
			want:     Result{ToolMatch: true, ActionMatch: true, ParamsValid: true, AnnotationRespected: true, Score: 1},
		},
		{
			name:     "wrong tool",
			scenario: readScenario,
			prop:     Proposal{Tool: "t_cards", Action: "get_board", Params: map[string]any{"board_id": "42"}},
			want:     Result{ToolMatch: false, ActionMatch: false, ParamsValid: false, AnnotationRespected: true, Score: 0},
		},
		{
			name:     "wrong action",
			scenario: readScenario,
			prop:     Proposal{Tool: "t_boards", Action: "delete_board", Params: map[string]any{"board_id": "42"}},
			// delete_board is destructive; the request was read-only-framed.
			want: Result{ToolMatch: true, ActionMatch: false, ParamsValid: true, AnnotationRespected: false, Score: 0},
		},
		{
			name:     "missing required param",
			scenario: readScenario,
			prop:     Proposal{Tool: "t_boards", Action: "get_board", Params: map[string]any{}},
			want:     Result{ToolMatch: true, ActionMatch: true, ParamsValid: false, AnnotationRespected: true, Score: 0},
		},
		{
			name:     "unknown param",
			scenario: readScenario,
			prop:     Proposal{Tool: "t_boards", Action: "get_board", Params: map[string]any{"board_id": "42", "bogus": 1}},
			want:     Result{ToolMatch: true, ActionMatch: true, ParamsValid: false, AnnotationRespected: true, Score: 0},
		},
		{
			name: "enum violation",
			scenario: Scenario{
				ID: "t_cards.create_card", Class: ClassWrite, GoldTool: "t_cards", GoldAction: "create_card",
				GoldParams: map[string]any{"board_id": "1", "title": "x"},
			},
			prop: Proposal{Tool: "t_cards", Action: "create_card", Params: map[string]any{
				"board_id": "1", "title": "x", "status": "archived"}},
			want: Result{ToolMatch: true, ActionMatch: true, ParamsValid: false, AnnotationRespected: true, Score: 0},
		},
		{
			name:     "unknown action",
			scenario: readScenario,
			prop:     Proposal{Tool: "t_boards", Action: "teleport", Params: map[string]any{}},
			want:     Result{ToolMatch: true, ActionMatch: false, ParamsValid: false, AnnotationRespected: true, Score: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Grade(tc.scenario, tc.prop, idx)
			if got.ToolMatch != tc.want.ToolMatch || got.ActionMatch != tc.want.ActionMatch ||
				got.ParamsValid != tc.want.ParamsValid || got.AnnotationRespected != tc.want.AnnotationRespected ||
				got.Score != tc.want.Score {
				t.Fatalf("Grade mismatch\n want %+v\n got  %+v\n reasons=%v", tc.want, got, got.Reasons)
			}
		})
	}
}

func TestGradeRequiresRequestedValues(t *testing.T) {
	idx := Index(fixtureSpecs())
	// update_board is an idempotent write: name is optional in the schema, but
	// the scenario asked to set it, so a schema-valid call that keeps only the
	// id has not done what was requested.
	sc := Scenario{
		ID: "t_boards.update_board", Class: ClassIdempotent, GoldTool: "t_boards", GoldAction: "update_board",
		GoldParams: map[string]any{"board_id": "5", "name": "Q3 Roadmap"},
	}
	idsOnly := Proposal{Tool: "t_boards", Action: "update_board", Params: map[string]any{"board_id": "5"}}
	if r := Grade(sc, idsOnly, idx); r.ParamsValid {
		t.Fatal("a call that dropped the requested mutation was graded params-valid")
	}
	wrongValue := Proposal{Tool: "t_boards", Action: "update_board", Params: map[string]any{"board_id": "5", "name": "Something else"}}
	if r := Grade(sc, wrongValue, idx); r.ParamsValid {
		t.Fatal("a call with a different value than requested was graded params-valid")
	}
	wrongResource := Proposal{Tool: "t_boards", Action: "update_board", Params: map[string]any{"board_id": "9", "name": "Q3 Roadmap"}}
	if r := Grade(sc, wrongResource, idx); r.ParamsValid {
		t.Fatal("a call targeting a different id was graded params-valid")
	}
	exact := Proposal{Tool: "t_boards", Action: "update_board", Params: map[string]any{"board_id": "5", "name": "Q3 Roadmap"}}
	if r := Grade(sc, exact, idx); !r.ParamsValid {
		t.Fatalf("the requested call was rejected: %v", r.Reasons)
	}
}

func TestGradeReadFramedRejectsAnyWrite(t *testing.T) {
	idx := Index(fixtureSpecs())
	// A read-only-framed request misrouted to a plain (non-destructive) write is
	// still a side effect the request never asked for.
	sc := Scenario{
		ID: "t_boards.get_board", Class: ClassRead, GoldTool: "t_boards", GoldAction: "get_board",
		GoldParams: map[string]any{"board_id": "1"}, ReadOnlyFramed: true,
	}
	write := Proposal{Tool: "t_cards", Action: "create_card", Params: map[string]any{"board_id": "1", "title": "x"}}
	if r := Grade(sc, write, idx); r.AnnotationRespected {
		t.Fatal("a plain write chosen for a read-only-framed request was graded safe")
	}
}

func TestValidateParamTypes(t *testing.T) {
	spec := ActionSpec{Params: []ParamSpec{
		{Name: "n", Type: "integer"},
		{Name: "flag", Type: "boolean"},
		{Name: "tags", Type: "array"},
	}}
	if ok, r := validateParams(spec, map[string]any{"n": 3.0, "flag": true, "tags": []any{"a"}}); !ok {
		t.Fatalf("valid params rejected: %v", r)
	}
	if ok, _ := validateParams(spec, map[string]any{"n": 3.5}); ok {
		t.Fatal("non-integral float accepted for integer param")
	}
	if ok, _ := validateParams(spec, map[string]any{"flag": "yes"}); ok {
		t.Fatal("string accepted for boolean param")
	}
}

func TestParseProposalToleratesFencesAndProse(t *testing.T) {
	raw := "Sure! Here you go:\n```json\n{\"tool\":\"t_boards\",\"action\":\"get_board\",\"params\":{\"board_id\":\"7\"}}\n```\nHope that helps."
	p, err := ParseProposal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tool != "t_boards" || p.Action != "get_board" || p.Params["board_id"] != "7" {
		t.Fatalf("bad parse: %+v", p)
	}
	if _, err := ParseProposal("no json here"); err == nil {
		t.Fatal("expected error on prose-only output")
	}
}

func TestParseProposalSkipsEarlierBraceBlock(t *testing.T) {
	// Prose contains a balanced brace block (an aside) before the real answer.
	raw := "I considered a note {see previous} and settled on:\n" +
		"```json\n{\"tool\":\"t_boards\",\"action\":\"get_board\",\"params\":{\"board_id\":\"7\"}}\n```"
	p, err := ParseProposal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tool != "t_boards" || p.Action != "get_board" || p.Params["board_id"] != "7" {
		t.Fatalf("scanned to the wrong object: %+v", p)
	}
}

func TestCostAndEstimate(t *testing.T) {
	if got := EstimateTokens("abcd"); got != 1 {
		t.Fatalf("EstimateTokens(4 chars)=%d, want 1", got)
	}
	p := Pricing{InputPerMTok: 1, OutputPerMTok: 2}
	if got := p.Cost(Usage{InputTokens: 1_000_000, OutputTokens: 500_000}); got != 2.0 {
		t.Fatalf("Cost=%v, want 2.0", got)
	}
}

// TestLoopEndToEndHermetic is the CI smoke: the whole loop against the
// in-process fake server with the deterministic oracle — no network, no model
// spend — proving spec derivation, generation, prompting, parsing, grading, and
// reporting all connect.
func TestLoopEndToEndHermetic(t *testing.T) {
	ctx := context.Background()
	srv, err := NewFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	session, cleanup, err := ConnectInProcess(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	specs, err := SpecFromSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		t.Fatal("no specs derived from fake server")
	}
	scenarios := Generate(specs, GenerateOptions{N: 8, Seed: 1})

	rep, err := Run(ctx, session, Config{
		Models:    []Model{NewOracleModel(scenarios)},
		Scenarios: scenarios,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Records) != len(scenarios) {
		t.Fatalf("want %d records, got %d", len(scenarios), len(rep.Records))
	}
	for _, r := range rep.Records {
		if r.Score < 1 { // the oracle must score every scenario
			t.Fatalf("oracle failed %s: params=%v safety=%v err=%q", r.ScenarioID, r.ParamsMatch, r.AnnotationRespected, r.Error)
		}
		// The oracle is the zero-spend backend: tokens are still measured, but
		// its priced cost must be exactly zero.
		if r.InTokens <= 0 {
			t.Fatalf("record %s has no measured tokens", r.ScenarioID)
		}
		if r.CostUSD != 0 {
			t.Fatalf("record %s reported nonzero oracle cost %v", r.ScenarioID, r.CostUSD)
		}
	}
	out := rep.Render("fake")
	if !strings.Contains(out, "TOTAL COST") || !strings.Contains(out, "PASS") {
		t.Fatalf("report missing table/totals:\n%s", out)
	}
}

func TestNonStringEnumValidation(t *testing.T) {
	spec := ActionSpec{Params: []ParamSpec{{Name: "level", Type: "integer", Enum: []any{1.0, 2.0}}}}
	if ok, r := validateParams(spec, map[string]any{"level": 2.0}); !ok {
		t.Fatalf("valid integer enum rejected: %v", r)
	}
	if ok, _ := validateParams(spec, map[string]any{"level": 3.0}); ok {
		t.Fatal("integer outside enum accepted")
	}
}

func TestObjectParamGeneratesObject(t *testing.T) {
	spec := ActionSpec{Tool: "t", Action: "set_meta",
		Params: []ParamSpec{{Name: "meta", In: "body", Required: true, Type: "object"}}}
	idx := Index([]ActionSpec{spec})
	for _, sc := range Generate([]ActionSpec{spec}, GenerateOptions{N: 1, Seed: 3}) {
		if _, ok := sc.GoldParams["meta"].(map[string]any); !ok {
			t.Fatalf("object param not generated as object: %#v", sc.GoldParams["meta"])
		}
		gold := Proposal{Tool: sc.GoldTool, Action: sc.GoldAction, Params: sc.GoldParams}
		if !Grade(sc, gold, idx).Pass() {
			t.Fatalf("object-param gold did not pass")
		}
	}
}

func TestNullParamGeneratesNil(t *testing.T) {
	spec := ActionSpec{Tool: "t", Action: "clear_field",
		Params: []ParamSpec{{Name: "value", In: "body", Required: true, Type: "null"}}}
	idx := Index([]ActionSpec{spec})
	for _, sc := range Generate([]ActionSpec{spec}, GenerateOptions{N: 1, Seed: 7}) {
		if v, ok := sc.GoldParams["value"]; !ok || v != nil {
			t.Fatalf("null param not generated as nil: %#v (present=%v)", v, ok)
		}
		gold := Proposal{Tool: sc.GoldTool, Action: sc.GoldAction, Params: sc.GoldParams}
		if !Grade(sc, gold, idx).Pass() {
			t.Fatal("null-param gold did not pass its own grading")
		}
	}
}

func TestNullTypeRejectsNonNil(t *testing.T) {
	if typeMatches("null", "x") {
		t.Fatal("null type accepted a string")
	}
	if !typeMatches("null", nil) {
		t.Fatal("null type rejected nil")
	}
}

func TestPricing(t *testing.T) {
	if p, ok := PricingFor("oracle"); !ok || p.Cost(Usage{InputTokens: 1e6, OutputTokens: 1e6}) != 0 {
		t.Fatalf("oracle must price at zero, got %+v ok=%v", p, ok)
	}
	if _, ok := PricingFor("mystery-model"); ok {
		t.Fatal("unknown label should not be found in the pricing table")
	}
}

func TestDuplicateFramingsDropped(t *testing.T) {
	// Two actions with identical summaries and no params frame identically. A
	// real model cannot tell them apart from the request, so the corpus keeps
	// one rather than emitting a coin-flip cell tagged with an ordinal or —
	// worse — with the gold action name the catalog prompt exposes.
	specs := []ActionSpec{
		{Tool: "t", Action: "zephyr", Summary: "Do the thing"},
		{Tool: "t", Action: "quokka", Summary: "Do the thing"},
		{Tool: "t", Action: "third", Summary: "Do the last thing"},
	}
	scen := Generate(specs, GenerateOptions{N: 3, Seed: 1})
	if len(scen) != 2 {
		t.Fatalf("want the collision dropped (2 scenarios), got %d: %+v", len(scen), scen)
	}
	seen := map[string]bool{}
	for _, s := range scen {
		if seen[s.NLFraming] {
			t.Fatalf("duplicate framing survived: %q", s.NLFraming)
		}
		seen[s.NLFraming] = true
		if strings.Contains(s.NLFraming, s.GoldAction) || strings.HasSuffix(s.NLFraming, ")") {
			t.Fatalf("framing %q was disguised instead of the collision being dropped", s.NLFraming)
		}
	}
}

func TestFailingRecords(t *testing.T) {
	records := []Record{
		{ScenarioID: "a", Score: 1},
		{ScenarioID: "b", Score: 0},
		{ScenarioID: "c", Score: 1, Error: "boom"},
	}
	failing := FailingRecords(records)
	if len(failing) != 2 {
		t.Fatalf("want 2 failing records, got %d", len(failing))
	}
}

func TestLoadCorpusPreservesServer(t *testing.T) {
	data, err := MarshalScenarios("fizzy", GenerateOptions{N: 3, Seed: 1}, Generate(fixtureSpecs(), GenerateOptions{N: 3, Seed: 1}))
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadCorpus(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "fizzy" {
		t.Fatalf("corpus server not preserved: %q", c.Server)
	}
	if len(c.Scenarios) == 0 {
		t.Fatal("corpus scenarios not loaded")
	}
}

// TestLoadCorpusRejectsEmpty pins the fail-closed rule: an empty scenario list
// unmarshals into a non-nil slice, which suppresses generation and leaves the
// run with nothing to grade — and a --require-pass gate with nothing to fail.
func TestLoadCorpusRejectsEmpty(t *testing.T) {
	if _, err := LoadCorpus([]byte(`{"server":"fake","seed":1,"n":0,"scenarios":[]}`)); err == nil {
		t.Fatal("empty scenario list accepted; a corpus with no scenarios must be rejected")
	}
	if _, err := LoadCorpus([]byte(`{"server":"fake"}`)); err == nil {
		t.Fatal("corpus with no scenarios key accepted; it must be rejected")
	}
	// A one-scenario corpus still loads.
	c, err := LoadCorpus([]byte(`{"server":"fake","scenarios":[{"scenario_id":"t.a","gold_tool":"t","gold_action":"a"}]}`))
	if err != nil {
		t.Fatalf("valid corpus rejected: %v", err)
	}
	if len(c.Scenarios) != 1 {
		t.Fatalf("want 1 scenario, got %d", len(c.Scenarios))
	}
}

// TestUnknownPricingIsStampedEstimated covers the design default: an unknown
// model label keeps the Haiku floor so ad-hoc runs still price, but the record
// and the report say the figure is estimated rather than measured.
func TestUnknownPricingIsStampedEstimated(t *testing.T) {
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	cost, estimated := costOf("mystery-model", u)
	if !estimated {
		t.Fatal("unknown label priced as measured; it must be stamped estimated")
	}
	if want := pricingTable["haiku"].Cost(u); cost != want {
		t.Fatalf("unknown label cost=%v, want the Haiku floor %v", cost, want)
	}

	if cost, estimated := costOf("haiku", u); estimated || cost == 0 {
		t.Fatalf("known label mis-stamped: cost=%v estimated=%v", cost, estimated)
	}
	if cost, estimated := costOf("oracle", u); estimated || cost != 0 {
		t.Fatalf("oracle mis-stamped: cost=%v estimated=%v", cost, estimated)
	}

	// The mark reaches the record and the rendered cost line.
	rep := &Report{
		Scenarios: []Scenario{{ID: "t.a", Class: ClassRead}},
		Records: []Record{{
			Model: "mystery-model", ScenarioID: "t.a", Class: ClassRead,
			Score: 1, InTokens: 10, OutTokens: 10, CostUSD: 0.001, PricingEstimated: true,
		}},
	}
	// The mark must ride the cost figures themselves — the per-model row and
	// the TOTAL COST line — not only a footnote, so a figure can never be read
	// as measured wherever it appears.
	out := rep.Render("fake")
	if line := lineWithPrefix(out, "TOTAL COST:"); !strings.Contains(line, "(estimated)") {
		t.Fatalf("TOTAL COST line does not mark estimated pricing: %q\n%s", line, out)
	}
	if line := lineWithPrefix(out, "mystery-model"); !strings.Contains(line, "(estimated)") {
		t.Fatalf("model totals row does not mark estimated pricing: %q\n%s", line, out)
	}

	known := &Report{
		Scenarios: []Scenario{{ID: "t.a", Class: ClassRead}},
		Records: []Record{{
			Model: "haiku", ScenarioID: "t.a", Class: ClassRead,
			Score: 1, InTokens: 10, OutTokens: 10, CostUSD: 0.001,
		}},
	}
	if out := known.Render("fake"); strings.Contains(out, "(estimated)") {
		t.Fatalf("known pricing marked estimated:\n%s", out)
	}
}

// lineWithPrefix returns the first line of s starting with prefix, or "".
func lineWithPrefix(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// TestRecordCarriesPricingEstimated proves the stamp survives the record's JSON
// encoding, so a results file records how its cost figures were priced.
func TestRecordCarriesPricingEstimated(t *testing.T) {
	var b strings.Builder
	if err := WriteJSONL(&b, []Record{{Model: "mystery-model", ScenarioID: "t.a", PricingEstimated: true}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"pricing_estimated":true`) {
		t.Fatalf("record JSON lacks the estimated-pricing stamp: %s", b.String())
	}
	// Measured pricing stays out of the file rather than writing false.
	b.Reset()
	if err := WriteJSONL(&b, []Record{{Model: "haiku", ScenarioID: "t.a"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "pricing_estimated") {
		t.Fatalf("measured record carries the stamp: %s", b.String())
	}
}

// TestRequirePassRejectsEmptyRun pins the other half of the fail-closed gate:
// with no records there is nothing to fail, so "no failures" must not read as
// a pass.
func TestRequirePassRejectsEmptyRun(t *testing.T) {
	if err := RequirePass(nil); err == nil {
		t.Fatal("a run with no records passed the gate")
	}
	if err := RequirePass([]Record{}); err == nil {
		t.Fatal("a run with an empty record slice passed the gate")
	}
	if err := RequirePass([]Record{{ScenarioID: "t.a", Score: 1}}); err != nil {
		t.Fatalf("a passing record was rejected: %v", err)
	}
	if err := RequirePass([]Record{{ScenarioID: "t.a", Score: 0}}); err == nil {
		t.Fatal("a failing record passed the gate")
	}
	if err := RequirePass([]Record{{ScenarioID: "t.a", Score: 1, Error: "boom"}}); err == nil {
		t.Fatal("an errored record passed the gate")
	}
}

// TestFramingRendersStructuredValuesAsJSON pins that object and array golds
// reach the model as JSON, not as Go's `map[]` / `[value]` rendering, which no
// JSON-speaking model would reproduce.
func TestFramingRendersStructuredValuesAsJSON(t *testing.T) {
	spec := ActionSpec{Tool: "t", Action: "set_meta", Summary: "Set the meta",
		Params: []ParamSpec{
			{Name: "meta", In: "body", Required: true, Type: "object"},
			{Name: "tags", In: "body", Required: true, Type: "array"}}}
	scen := Generate([]ActionSpec{spec}, GenerateOptions{N: 1, Seed: 3})
	if len(scen) != 1 {
		t.Fatalf("want 1 scenario, got %d", len(scen))
	}
	f := scen[0].NLFraming
	if strings.Contains(f, "map[") || !strings.Contains(f, "meta {}") {
		t.Fatalf("object gold not rendered as JSON: %q", f)
	}
	if !strings.Contains(f, `tags ["Tags"]`) {
		t.Fatalf("array gold not rendered as JSON: %q", f)
	}
}

// TestParseProposalPrefersCompleteProposal pins that an earlier partial object
// in prose (an echo of the {"action": ...} call shape, say) does not mask the
// complete answer that follows it.
func TestParseProposalPrefersCompleteProposal(t *testing.T) {
	raw := "The call shape is {\"action\":\"get_board\"}, so:\n" +
		"```json\n{\"tool\":\"t_boards\",\"action\":\"get_board\",\"params\":{\"board_id\":\"7\"}}\n```"
	p, err := ParseProposal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tool != "t_boards" || p.Action != "get_board" || p.Params["board_id"] != "7" {
		t.Fatalf("partial object masked the complete proposal: %+v", p)
	}
	// With no complete candidate, a partial one still beats a bare object.
	p, err = ParseProposal("{\"note\":1} then {\"action\":\"get_board\"}")
	if err != nil {
		t.Fatal(err)
	}
	if p.Action != "get_board" {
		t.Fatalf("partial proposal not returned as fallback: %+v", p)
	}
}

// TestRunRejectsDuplicateModelLabels pins that two models sharing a label
// refuse to run: records, report cells, and baseline keys identify a model by
// label, so a duplicate would overwrite cells while totals charged both.
func TestRunRejectsDuplicateModelLabels(t *testing.T) {
	ctx := context.Background()
	srv, err := NewFakeServer()
	if err != nil {
		t.Fatal(err)
	}
	session, cleanup, err := ConnectInProcess(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	specs, err := SpecFromSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	scenarios := Generate(specs, GenerateOptions{N: 2, Seed: 1})
	_, err = Run(ctx, session, Config{
		Models:    []Model{NewOracleModel(scenarios), NewOracleModel(scenarios)},
		Scenarios: scenarios,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate model label") {
		t.Fatalf("duplicate labels accepted: err=%v", err)
	}
	if _, err := Run(ctx, session, Config{Scenarios: scenarios}); err == nil {
		t.Fatal("a run with no models was accepted")
	}
}

// TestRunRejectsEmptyCorpus pins the generation-side half of the fail-closed
// rule: a server whose catalog exposes no actions generates no scenarios, and
// that must be an error, not a green zero-cell run.
func TestRunRejectsEmptyCorpus(t *testing.T) {
	ctx := context.Background()
	empty := mcp.NewServer(&mcp.Implementation{Name: "empty-mcp-server", Version: "0.0.0"}, nil)
	session, cleanup, err := ConnectInProcess(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	_, err = Run(ctx, session, Config{Models: []Model{NewOracleModel(nil)}})
	if err == nil || !strings.Contains(err.Error(), "no scenarios") {
		t.Fatalf("empty generated corpus accepted: err=%v", err)
	}
	// A caller-supplied empty (non-nil) corpus is refused the same way.
	_, err = Run(ctx, session, Config{Models: []Model{NewOracleModel(nil)}, Scenarios: []Scenario{}})
	if err == nil || !strings.Contains(err.Error(), "no scenarios") {
		t.Fatalf("empty supplied corpus accepted: err=%v", err)
	}
}

// TestCatalogRendersEnumMembersAsJSON pins that the catalog shows enum members
// as the JSON values the grader compares against: a string "1" and a number 1
// must not both read as 1.
func TestCatalogRendersEnumMembersAsJSON(t *testing.T) {
	out := BuildSystem([]ActionSpec{{Tool: "t", Action: "a", Summary: "Do it", Params: []ParamSpec{
		{Name: "mode", In: "body", Type: "string", Enum: []any{"1", "fast"}},
		{Name: "level", In: "body", Type: "integer", Enum: []any{1.0, 2.0}},
	}}})
	if !strings.Contains(out, `mode(enum: "1"|"fast")`) {
		t.Fatalf("string enum members not quoted as JSON:\n%s", out)
	}
	if !strings.Contains(out, `level(enum: 1|2)`) {
		t.Fatalf("numeric enum members not rendered as JSON numbers:\n%s", out)
	}
}
