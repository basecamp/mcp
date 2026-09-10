package eval

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/gateway"
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

// countingModel records how many proposals were requested, so a test can prove
// a run aborted before the model loop rather than after burning it.
type countingModel struct{ calls int }

func (m *countingModel) Label() string   { return "counting" }
func (m *countingModel) ModelID() string { return "counting-v1" }
func (m *countingModel) Propose(context.Context, string, string) (string, Usage, error) {
	m.calls++
	return `{"tool":"","action":"","params":{}}`, Usage{}, nil
}

// TestRunRejectsObsoleteGoldBeforeSpend pins that a corpus whose gold can no
// longer be satisfied by the live catalog — a removed action, or a gold that
// the current schema rejects — aborts before any model call, as a
// corpus/surface mismatch, instead of charging every model for cells that
// cannot pass and reporting the misses as model failures.
func TestRunRejectsObsoleteGoldBeforeSpend(t *testing.T) {
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
	good := Generate(specs, GenerateOptions{N: 3, Seed: 1})

	cases := map[string]func([]Scenario){
		"removed action": func(s []Scenario) { s[0].GoldAction = "no_such_action" },
		"removed tool":   func(s []Scenario) { s[0].GoldTool = "no_such_tool" },
		"gold rejected by current schema": func(s []Scenario) {
			s[0].GoldParams = map[string]any{"bogus_param": 1}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			corpus := append([]Scenario(nil), good...)
			mutate(corpus)
			model := &countingModel{}
			_, err := Run(ctx, session, Config{Models: []Model{model}, Scenarios: corpus})
			if err == nil {
				t.Fatal("obsolete gold accepted; the run should abort as a corpus/surface mismatch")
			}
			if !strings.Contains(err.Error(), corpus[0].ID) {
				t.Fatalf("error does not name the scenario: %v", err)
			}
			if model.calls != 0 {
				t.Fatalf("model was called %d times before the corpus check", model.calls)
			}
		})
	}

	// The unmodified corpus still runs.
	model := &countingModel{}
	if _, err := Run(ctx, session, Config{Models: []Model{model}, Scenarios: good}); err != nil {
		t.Fatalf("valid corpus rejected: %v", err)
	}
	if model.calls != len(good) {
		t.Fatalf("want %d model calls, got %d", len(good), model.calls)
	}
}

// TestRunRejectsDuplicateScenarioIDs pins the fail-closed rule for a corpus
// carrying one scenario ID twice: cells key on the ID, so the second would
// overwrite the first's cell while the totals counted and charged both.
func TestRunRejectsDuplicateScenarioIDs(t *testing.T) {
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
	corpus := Generate(specs, GenerateOptions{N: 2, Seed: 1})
	dup := corpus[0]
	dup.NLFraming = "Another phrasing of the same task."
	corpus = append(corpus, dup)
	model := &countingModel{}
	_, err = Run(ctx, session, Config{Models: []Model{model}, Scenarios: corpus})
	if err == nil || !strings.Contains(err.Error(), "duplicate scenario id") {
		t.Fatalf("duplicate scenario id accepted: err=%v", err)
	}
	if model.calls != 0 {
		t.Fatalf("model was called %d times before the corpus check", model.calls)
	}
}

// TestRunRejectsDuplicateFramings pins that a loaded corpus with unique ids
// but one framing under two golds is refused before spend: the oracle keys on
// the framing, and a real model gets identical prompts with conflicting
// expected answers, so one cell can never pass.
func TestRunRejectsDuplicateFramings(t *testing.T) {
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
	corpus := Generate(specs, GenerateOptions{N: 2, Seed: 1})
	corpus[1].NLFraming = corpus[0].NLFraming
	model := &countingModel{}
	_, err = Run(ctx, session, Config{Models: []Model{model}, Scenarios: corpus})
	if err == nil || !strings.Contains(err.Error(), "share a framing") {
		t.Fatalf("duplicate framing accepted: err=%v", err)
	}
	if !strings.Contains(err.Error(), corpus[0].ID) || !strings.Contains(err.Error(), corpus[1].ID) {
		t.Fatalf("error does not name both scenarios: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("model was called %d times before the corpus check", model.calls)
	}
}

// TestRunRejectsSafetyDriftBeforeSpend pins that pinned safety metadata is
// checked against the live catalog before any model call. Grading cannot see
// this: the exact gold would fail the safety dimension (read-only framing on
// an action that is no longer read-only), or the reverse drift would inflate
// the safety rate — either way after the cells were paid for.
func TestRunRejectsSafetyDriftBeforeSpend(t *testing.T) {
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
	pinned := Generate(specs, GenerateOptions{N: 100, Seed: 1})
	find := func(want Class) int {
		for i, sc := range pinned {
			if sc.Class == want {
				return i
			}
		}
		t.Fatalf("fake catalog has no %s action", want)
		return -1
	}
	cases := map[string]func([]Scenario){
		"read action pinned as write": func(s []Scenario) {
			i := find(ClassRead)
			s[i].Class = ClassWrite
			s[i].ReadOnlyFramed = false
		},
		"write action pinned as read-only": func(s []Scenario) {
			i := find(ClassWrite)
			s[i].Class = ClassRead
			s[i].ReadOnlyFramed = true
		},
		"idempotent write pinned as plain write": func(s []Scenario) { s[find(ClassIdempotent)].Class = ClassWrite },
		"destructive pinned as idempotent":       func(s []Scenario) { s[find(ClassDestructive)].Class = ClassIdempotent },
		"readonly_framed alone drifted":          func(s []Scenario) { s[find(ClassRead)].ReadOnlyFramed = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			corpus := append([]Scenario(nil), pinned...)
			mutate(corpus)
			model := &countingModel{}
			_, err := Run(ctx, session, Config{Models: []Model{model}, Scenarios: corpus})
			if err == nil || !strings.Contains(err.Error(), "drifted") {
				t.Fatalf("safety drift accepted: err=%v", err)
			}
			if model.calls != 0 {
				t.Fatalf("model was called %d times before the corpus check", model.calls)
			}
		})
	}
	if _, err := Run(ctx, session, Config{Models: []Model{&countingModel{}}, Scenarios: pinned}); err != nil {
		t.Fatalf("matching corpus rejected: %v", err)
	}
}

// TestRecordCarriesResolvedModelID pins that every record names the wire
// model behind its label, and that the id survives the JSONL encoding, so two
// results files labelled "haiku" from different underlying models stay
// distinguishable after a rolling alias is retargeted.
func TestRecordCarriesResolvedModelID(t *testing.T) {
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
	rep, err := Run(ctx, session, Config{Models: []Model{&countingModel{}, NewOracleModel(scenarios)}, Scenarios: scenarios})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Records {
		want := map[string]string{"counting": "counting-v1", "oracle": "oracle"}[r.Model]
		if r.ModelID != want {
			t.Fatalf("record for %s/%s carries model_id %q, want %q", r.Model, r.ScenarioID, r.ModelID, want)
		}
	}
	var b strings.Builder
	if err := WriteJSONL(&b, rep.Records[:1]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"model_id":"counting-v1"`) {
		t.Fatalf("model_id missing from the JSONL record: %s", b.String())
	}
	// The backends resolve their ids: the API model carries the wire id, the
	// CLI model the alias it passes to --model.
	if got := NewCLIModel("haiku", "haiku").ModelID(); got != "haiku" {
		t.Fatalf("CLI model id = %q", got)
	}
}

// TestOptionalBodyKeepsInnerRequiredConditional pins that flattening an
// optional request body does not promote its schema's required properties to
// unconditional: a gold or proposal that omits the whole body is valid, one
// that sends a body must carry them, and a required body still binds them
// outright. Generation honors the same rule so a gold never fails its own
// validation.
func TestOptionalBodyKeepsInnerRequiredConditional(t *testing.T) {
	body := map[string]any{"type": "object", "required": []any{"name"},
		"properties": map[string]any{"name": fakeStr("Name"), "color": fakeStr("Color")}}

	optional := ActionSpec{Tool: "t", Action: "update_thing", Summary: "Update a thing", Idempotent: true,
		Params: append([]ParamSpec{{Name: "thing_id", In: "path", Required: true, Type: "integer"}}, bodyParams(body, false)...)}
	if got := optional.RequiredParams(); len(got) != 1 || got[0] != "thing_id" {
		t.Fatalf("optional body promoted inner required to unconditional: %v", got)
	}
	if ok, r := validateParams(optional, map[string]any{"thing_id": 1.0}); !ok {
		t.Fatalf("omitting an optional body rejected: %v", r)
	}
	if ok, _ := validateParams(optional, map[string]any{"thing_id": 1.0, "color": "blue"}); ok {
		t.Fatal("a body missing its required property accepted")
	}
	if ok, r := validateParams(optional, map[string]any{"thing_id": 1.0, "color": "blue", "name": "x"}); !ok {
		t.Fatalf("a complete body rejected: %v", r)
	}

	required := ActionSpec{Tool: "t", Action: "create_thing", Summary: "Create a thing",
		Params: bodyParams(body, true)}
	if ok, _ := validateParams(required, map[string]any{}); ok {
		t.Fatal("a required body's required property was not enforced")
	}

	// A generated gold for the optional-body action carries a mutation and
	// therefore the body's required property; it validates against itself.
	idx := Index([]ActionSpec{optional})
	for _, sc := range Generate([]ActionSpec{optional}, GenerateOptions{N: 1, Seed: 2}) {
		if _, ok := sc.GoldParams["name"]; !ok {
			t.Fatalf("gold sent a body without its required property: %v", sc.GoldParams)
		}
		if ok, r := validateParams(optional, sc.GoldParams); !ok {
			t.Fatalf("gold fails its own validation: %v", r)
		}
		if !Grade(sc, Proposal{Tool: sc.GoldTool, Action: sc.GoldAction, Params: sc.GoldParams}, idx).Pass() {
			t.Fatal("gold proposal did not pass")
		}
	}

	// A pinned corpus whose gold omits the optional body passes the preflight
	// and grades cleanly: the catalog prompt must not tell the model the
	// field is mandatory either.
	sc := Scenario{ID: "t.update_thing", Class: ClassIdempotent, NLFraming: "Please update a thing (thing id 7).",
		GoldTool: "t", GoldAction: "update_thing", GoldParams: map[string]any{"thing_id": 7}}
	if err := checkCorpus([]Scenario{sc}, idx); err != nil {
		t.Fatalf("valid body-less gold rejected by preflight: %v", err)
	}
	if out := BuildSystem([]ActionSpec{optional}); strings.Contains(out, "name*") || !strings.Contains(out, "required with body") {
		t.Fatalf("catalog prompt misstates the body requirement:\n%s", out)
	}
}

// TestFakeServerCarriesBodyRequired pins the wire: the fake's creates declare
// a required body, so their inner required properties arrive unconditional
// through SpecFromSession, while an optional body's do not.
func TestFakeServerCarriesBodyRequired(t *testing.T) {
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
	idx := Index(specs)
	create, _ := idx.lookup("fake_boards", "create_board")
	if p, _ := create.param("name"); !p.Required || p.RequiredWithBody {
		t.Fatalf("required body's property not unconditional: %+v", p)
	}
	update, _ := idx.lookup("fake_boards", "update_board")
	if p, _ := update.param("name"); p.Required || p.RequiredWithBody {
		t.Fatalf("optional, non-required body property mis-flagged: %+v", p)
	}
}

// TestEnumMembersCompareInJSONValueSpace pins that enum membership does not
// depend on Go type identity: a programmatically built spec's int member
// matches the float64 a JSON-decoded proposal carries, and a string "1" does
// not match the number 1.
func TestEnumMembersCompareInJSONValueSpace(t *testing.T) {
	spec := ActionSpec{Params: []ParamSpec{{Name: "level", Type: "integer", Enum: []any{1, 2}}}}
	if ok, r := validateParams(spec, map[string]any{"level": float64(1)}); !ok {
		t.Fatalf("decoded float64 rejected against int enum member: %v", r)
	}
	if ok, _ := validateParams(spec, map[string]any{"level": "1"}); ok {
		t.Fatal(`string "1" accepted against numeric enum`)
	}
	if ok, _ := validateParams(spec, map[string]any{"level": float64(3)}); ok {
		t.Fatal("number outside the enum accepted")
	}
	// The oracle's own gold round-trips through JSON, so a programmatic spec
	// with int enum members must grade its gold as a pass.
	idx := Index([]ActionSpec{{Tool: "t", Action: "set_level", Summary: "Set the level", Params: spec.Params}})
	sc := Scenario{ID: "t.set_level", GoldTool: "t", GoldAction: "set_level", GoldParams: map[string]any{"level": 2}}
	raw, _ := json.Marshal(Proposal{Tool: "t", Action: "set_level", Params: sc.GoldParams})
	prop, err := ParseProposal(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if r := Grade(sc, prop, idx); !r.Pass() {
		t.Fatalf("JSON-round-tripped gold failed against int enum: %v", r.Reasons)
	}
}

// TestRunRejectsEmptyIDsAndFramings pins two more corpus-shape refusals: a
// scenario without an id cannot key a cell, and one with an empty framing
// asks the model nothing while the oracle still answers it — a green cell
// that evaluated no routing.
func TestRunRejectsEmptyIDsAndFramings(t *testing.T) {
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
	good := Generate(specs, GenerateOptions{N: 2, Seed: 1})
	cases := map[string]struct {
		mutate func([]Scenario)
		want   string
	}{
		"empty id":           {func(s []Scenario) { s[1].ID = "  " }, "has no id"},
		"empty framing":      {func(s []Scenario) { s[0].NLFraming = "" }, "empty framing"},
		"whitespace framing": {func(s []Scenario) { s[0].NLFraming = " \n\t" }, "empty framing"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			corpus := append([]Scenario(nil), good...)
			tc.mutate(corpus)
			model := &countingModel{}
			_, err := Run(ctx, session, Config{Models: []Model{model}, Scenarios: corpus})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("accepted; err=%v", err)
			}
			if model.calls != 0 {
				t.Fatalf("model was called %d times before the corpus check", model.calls)
			}
		})
	}
}

// TestRecordExplainsFailureAndMarksEstimatedUsage pins the record's
// diagnostics: a failed cell carries the grader's reasons and the proposed
// params, a passing cell carries neither, and a cell whose token counts were
// estimated (the counting model reports none) says so — and the report marks
// its cost figures as estimated even at a published price.
func TestRecordExplainsFailureAndMarksEstimatedUsage(t *testing.T) {
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
	// The counting model proposes an empty tool/action: every cell fails.
	rep, err := Run(ctx, session, Config{Models: []Model{&countingModel{}, NewOracleModel(scenarios)}, Scenarios: scenarios})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Records {
		switch r.Model {
		case "counting":
			if len(r.Reasons) == 0 {
				t.Fatalf("failed cell %s carries no reasons", r.ScenarioID)
			}
			if r.ChoseParams == nil {
				t.Fatalf("failed cell %s carries no proposed params", r.ScenarioID)
			}
		case "oracle":
			if len(r.Reasons) != 0 {
				t.Fatalf("passing cell %s carries reasons: %v", r.ScenarioID, r.Reasons)
			}
		}
		if !r.UsageEstimated {
			t.Fatalf("cell %s/%s: backend reported no usage but the record is not marked estimated", r.Model, r.ScenarioID)
		}
	}
	var b strings.Builder
	if err := WriteJSONL(&b, rep.Records); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"reasons":[`) || !strings.Contains(b.String(), `"usage_estimated":true`) {
		t.Fatalf("diagnostics missing from JSONL: %s", b.String())
	}

	// A published price on estimated counts is still an estimate.
	haiku := &Report{Scenarios: scenarios[:1], Records: []Record{{
		Model: "haiku", ScenarioID: scenarios[0].ID, Score: 1, InTokens: 10, OutTokens: 5, CostUSD: 0.001, UsageEstimated: true,
	}}}
	out := haiku.Render("fake")
	if !strings.Contains(lineWithPrefix(out, "TOTAL COST:"), "(estimated)") || !strings.Contains(lineWithPrefix(out, "haiku"), "(estimated)") {
		t.Fatalf("estimated usage not marked on the cost figures:\n%s", out)
	}
	if !strings.Contains(out, "token counts the backend did not report") {
		t.Fatalf("estimated-usage footnote missing:\n%s", out)
	}
}

// TestCLIErrorKeepsReportedUsage pins that a CLI turn the CLI itself reports
// as an error still hands back the usage it consumed, so the runner prices
// the failed call at what was billed rather than at nothing.
func TestCLIErrorKeepsReportedUsage(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '{\"result\":\"boom\",\"is_error\":true,\"usage\":{\"input_tokens\":123,\"output_tokens\":4}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVAL_CLAUDE_BIN", script)
	_, usage, err := NewCLIModel("haiku", "haiku").Propose(context.Background(), "sys", "user")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("CLI error not surfaced: %v", err)
	}
	if usage.InputTokens != 123 || usage.OutputTokens != 4 {
		t.Fatalf("reported usage dropped on error: %+v", usage)
	}
}

// TestCommittedResultsCarryProvenance pins that every committed results file
// names the wire model behind its label, and marks estimated usage, so a
// baseline is never ambiguous about what produced it or how its cost was
// derived.
func TestCommittedResultsCarryProvenance(t *testing.T) {
	paths, err := filepath.Glob("results/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := filepath.Glob("testdata/results/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, pinned...)
	if len(paths) == 0 {
		t.Fatal("no committed results found")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var rec Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("%s:%d: %v", path, i+1, err)
			}
			if rec.ModelID == "" {
				t.Fatalf("%s:%d: record has no model_id", path, i+1)
			}
			if rec.Model != "oracle" && !rec.UsageEstimated && rec.InTokens == 0 {
				t.Fatalf("%s:%d: no tokens and not marked estimated", path, i+1)
			}
		}
	}
}

// lyingDomain advertises its real action names in the domain listing but
// returns a different action name from the per-action describe — a server
// whose two describe surfaces disagree.
type lyingDomain struct{ *fakeDomain }

func (d lyingDomain) Describe(action string) (any, error) {
	if action == "" {
		return d.fakeDomain.Describe("")
	}
	return &fakeOp{Action: "renamed_" + action, Summary: "x"}, nil
}

// TestSpecRejectsDescribeActionMismatch pins that spec derivation refuses a
// server whose per-action describe names a different action than the domain
// listing asked for, instead of silently substituting it (which would drop
// the listed action while a generated oracle still graded perfectly).
func TestSpecRejectsDescribeActionMismatch(t *testing.T) {
	honest, ok := fakeDomains()[0].(*fakeDomain)
	if !ok {
		t.Fatal("fakeDomains()[0] is not a *fakeDomain")
	}
	gw, err := gateway.New([]gateway.Domain{lyingDomain{honest}}, gateway.Config{
		Handler: func(context.Context, gateway.Domain, gateway.Operation, map[string]any) (*mcp.CallToolResult, error) {
			return gateway.JSONResult(map[string]any{"ok": true})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.BuildMCPServer(&mcp.Implementation{Name: "lying-mcp-server", Version: "0.0.0"}, nil)
	ctx := context.Background()
	session, cleanup, err := ConnectInProcess(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	_, err = SpecFromSession(ctx, session)
	if err == nil || !strings.Contains(err.Error(), "not the requested") {
		t.Fatalf("describe/listing mismatch accepted: err=%v", err)
	}
}

// TestGenerateDropsSummarylessActions pins that an action with no summary is
// dropped from generation: it cannot be framed (the request derives from the
// summary, and its own name is the answer), so it would otherwise produce a
// bare "Could you ." that exercises no routing yet still passes.
func TestGenerateDropsSummarylessActions(t *testing.T) {
	specs := []ActionSpec{
		{Tool: "t", Action: "usable", Summary: "Do the thing", ReadOnly: true},
		{Tool: "t", Action: "blank", Summary: "   "},
		{Tool: "t", Action: "empty", Summary: ""},
	}
	scen := Generate(specs, GenerateOptions{N: 10, Seed: 1})
	if len(scen) != 1 || scen[0].GoldAction != "usable" {
		t.Fatalf("summary-less actions not dropped: %+v", scen)
	}
	for _, s := range scen {
		bare := strings.TrimSuffix(strings.TrimPrefix(s.NLFraming, "Could you "), ".")
		if strings.TrimSpace(bare) == "" {
			t.Fatalf("degenerate framing survived: %q", s.NLFraming)
		}
	}
	if got := Generate([]ActionSpec{{Tool: "t", Action: "blank", Summary: ""}}, GenerateOptions{N: 5, Seed: 1}); len(got) != 0 {
		t.Fatalf("a catalog of only summary-less actions must yield nothing, got %d", len(got))
	}
}

// TestCaseHelpersHandleMultibyteRunes pins that the first-letter case change
// decodes a full UTF-8 rune, so a localized summary is not corrupted into an
// invalid byte sequence before it reaches the model.
func TestCaseHelpersHandleMultibyteRunes(t *testing.T) {
	if got := lowerFirst("Ärger"); got != "ärger" || !utf8.ValidString(got) {
		t.Fatalf("lowerFirst mangled a multibyte rune: %q", got)
	}
	if got := capitalize("übung"); got != "Übung" || !utf8.ValidString(got) {
		t.Fatalf("capitalize mangled a multibyte rune: %q", got)
	}
	scen := Generate([]ActionSpec{{Tool: "t", Action: "a", Summary: "Öffnen the board", ReadOnly: true,
		Params: []ParamSpec{{Name: "board_id", In: "path", Required: true, Type: "integer"}}}},
		GenerateOptions{N: 1, Seed: 1})
	if len(scen) != 1 {
		t.Fatalf("want 1 scenario, got %d", len(scen))
	}
	if !utf8.ValidString(scen[0].NLFraming) || strings.ContainsRune(scen[0].NLFraming, '�') {
		t.Fatalf("framing corrupted for a non-ASCII summary: %q", scen[0].NLFraming)
	}
}

// metaLiar advertises every action as read-only in the domain listing while
// its per-action detail keeps the real annotations — a server whose two
// describe surfaces disagree on shared metadata.
type metaLiar struct{ *fakeDomain }

func (d metaLiar) Describe(action string) (any, error) {
	if action == "" {
		acts := make([]map[string]any, 0, len(d.ops))
		for _, o := range d.ops {
			acts = append(acts, map[string]any{
				"action": o.Action, "summary": o.Summary,
				"readonly": true, "destructive": o.Destructive,
			})
		}
		return map[string]any{"domain": d.key, "actions": acts}, nil
	}
	return d.fakeDomain.Describe(action)
}

// TestSpecRejectsDomainDetailMetadataConflict pins that spec derivation refuses
// a server whose domain listing and per-action detail disagree on a shared
// field (here, readonly), instead of silently dropping the listing's copy and
// scoring against the detail while a client would read the other.
func TestSpecRejectsDomainDetailMetadataConflict(t *testing.T) {
	honest, ok := fakeDomains()[0].(*fakeDomain)
	if !ok {
		t.Fatal("fakeDomains()[0] is not a *fakeDomain")
	}
	gw, err := gateway.New([]gateway.Domain{metaLiar{honest}}, gateway.Config{
		Handler: func(context.Context, gateway.Domain, gateway.Operation, map[string]any) (*mcp.CallToolResult, error) {
			return gateway.JSONResult(map[string]any{"ok": true})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.BuildMCPServer(&mcp.Implementation{Name: "meta-liar", Version: "0.0.0"}, nil)
	ctx := context.Background()
	session, cleanup, err := ConnectInProcess(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := SpecFromSession(ctx, session); err == nil || !strings.Contains(err.Error(), "domain listing") {
		t.Fatalf("domain/detail metadata conflict accepted: err=%v", err)
	}
}

// TestEnumMemberMustSatisfyDeclaredType pins that an enum match no longer
// excuses the declared type: a non-integral member on an integer param, or a
// numeric member on a string param, is rejected — so generation cannot pick a
// schema-invalid member and have the oracle score it valid.
func TestEnumMemberMustSatisfyDeclaredType(t *testing.T) {
	ispec := ActionSpec{Params: []ParamSpec{{Name: "level", Type: "integer", Enum: []any{1.0, 1.5}}}}
	if ok, _ := validateParams(ispec, map[string]any{"level": 1.5}); ok {
		t.Fatal("non-integral enum member accepted for an integer param")
	}
	if ok, r := validateParams(ispec, map[string]any{"level": 1.0}); !ok {
		t.Fatalf("valid integer enum member rejected: %v", r)
	}
	sspec := ActionSpec{Params: []ParamSpec{{Name: "mode", Type: "string", Enum: []any{"a", 2.0}}}}
	if ok, _ := validateParams(sspec, map[string]any{"mode": 2.0}); ok {
		t.Fatal("numeric enum member accepted for a string param")
	}
	if ok, r := validateParams(sspec, map[string]any{"mode": "a"}); !ok {
		t.Fatalf("valid string enum member rejected: %v", r)
	}
}

// TestDynamicBodyAcceptsAdditionalProperties pins that a body declaring
// additionalProperties:true carries that permission into the spec, so the
// grader accepts fields the schema does not name instead of rejecting them as
// unknown — a dictionary body would otherwise be un-passable.
func TestDynamicBodyAcceptsAdditionalProperties(t *testing.T) {
	if !bodyAllowsAdditional(map[string]any{"type": "object", "additionalProperties": true}) {
		t.Fatal("explicit additionalProperties:true not recognized")
	}
	if bodyAllowsAdditional(map[string]any{"type": "object"}) {
		t.Fatal("absent additionalProperties treated as open")
	}
	if bodyAllowsAdditional(map[string]any{"additionalProperties": map[string]any{"type": "string"}}) {
		t.Fatal("a schema-valued additionalProperties must not read as open (v0)")
	}
	spec := ActionSpec{Tool: "t", Action: "set_meta", Summary: "Set meta", BodyDynamic: true,
		Params: []ParamSpec{{Name: "id", In: "path", Required: true, Type: "integer"}}}
	if ok, r := validateParams(spec, map[string]any{"id": 1, "anything": "x", "more": 2.0}); !ok {
		t.Fatalf("dynamic body rejected undeclared fields: %v", r)
	}
	spec.BodyDynamic = false
	if ok, _ := validateParams(spec, map[string]any{"id": 1, "anything": "x"}); ok {
		t.Fatal("a closed body accepted an undeclared field")
	}
}

// omitLiar drops its first action from the domain listing while the tools/list
// input schema still enumerates it — a server whose describe surface hides a
// routed action.
type omitLiar struct{ *fakeDomain }

func (d omitLiar) Describe(action string) (any, error) {
	if action == "" {
		acts := make([]map[string]any, 0, len(d.ops))
		for _, o := range d.ops[1:] { // omit the first action from the listing
			acts = append(acts, map[string]any{
				"action": o.Action, "summary": o.Summary,
				"readonly": o.ReadOnly, "destructive": o.Destructive,
			})
		}
		return map[string]any{"domain": d.key, "actions": acts}, nil
	}
	return d.fakeDomain.Describe(action)
}

// TestSpecRejectsToolEnumActionOmittedByDomain pins that a route advertised in
// the tools/list schema but missing from the domain describe fails spec
// derivation, rather than silently dropping from the corpus and never being
// evaluated.
func TestSpecRejectsToolEnumActionOmittedByDomain(t *testing.T) {
	honest, ok := fakeDomains()[0].(*fakeDomain)
	if !ok {
		t.Fatal("fakeDomains()[0] is not a *fakeDomain")
	}
	gw, err := gateway.New([]gateway.Domain{omitLiar{honest}}, gateway.Config{
		Handler: func(context.Context, gateway.Domain, gateway.Operation, map[string]any) (*mcp.CallToolResult, error) {
			return gateway.JSONResult(map[string]any{"ok": true})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.BuildMCPServer(&mcp.Implementation{Name: "omit-liar", Version: "0.0.0"}, nil)
	ctx := context.Background()
	session, cleanup, err := ConnectInProcess(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := SpecFromSession(ctx, session); err == nil || !strings.Contains(err.Error(), "omits it") {
		t.Fatalf("omitted routed action accepted: err=%v", err)
	}
}

// TestGenerateEnumHonorsDeclaredType pins that generation only ever chooses an
// enum member that satisfies the declared type, so no seed produces a gold
// that its own grader (which now type-checks enum members) would reject.
func TestGenerateEnumHonorsDeclaredType(t *testing.T) {
	if v, ok := pickEnum(newRand(1), ParamSpec{Type: "integer", Enum: []any{1.0, 1.5}}); !ok || v != 1.0 {
		t.Fatalf("pickEnum returned %v (ok=%v), want the only integral member 1", v, ok)
	}
	if _, ok := pickEnum(newRand(1), ParamSpec{Type: "integer", Enum: []any{1.5, 2.5}}); ok {
		t.Fatal("pickEnum returned a member though none satisfy the integer type")
	}
	spec := ActionSpec{Tool: "t", Action: "set_level", Summary: "Set the level", Idempotent: true,
		Params: []ParamSpec{{Name: "level", In: "body", Required: true, Type: "integer", Enum: []any{1.0, 1.5, 2.0}}}}
	idx := Index([]ActionSpec{spec})
	for seed := int64(0); seed < 50; seed++ {
		for _, sc := range Generate([]ActionSpec{spec}, GenerateOptions{N: 1, Seed: seed}) {
			if v := sc.GoldParams["level"]; v == 1.5 {
				t.Fatalf("seed %d generated the non-integral enum member 1.5", seed)
			}
			if !Grade(sc, Proposal{Tool: sc.GoldTool, Action: sc.GoldAction, Params: sc.GoldParams}, idx).Pass() {
				t.Fatalf("seed %d gold failed its own grading: %v", seed, sc.GoldParams)
			}
		}
	}
}

// newRand is a tiny helper mirroring the generator's rng construction.
func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

// TestGenerateMutatesDictionaryBody pins that a write action with an open
// dictionary body and no named properties still gets a body mutation, so its
// scenario exercises body routing instead of passing empty.
func TestGenerateMutatesDictionaryBody(t *testing.T) {
	spec := ActionSpec{Tool: "t", Action: "set_meta", Summary: "Set the metadata", Idempotent: true, BodyDynamic: true,
		Params: []ParamSpec{{Name: "id", In: "path", Required: true, Type: "integer"}}}
	idx := Index([]ActionSpec{spec})
	scen := Generate([]ActionSpec{spec}, GenerateOptions{N: 1, Seed: 1})
	if len(scen) != 1 {
		t.Fatalf("want 1 scenario, got %d", len(scen))
	}
	body := false
	for name := range scen[0].GoldParams {
		if name != "id" {
			body = true
		}
	}
	if !body {
		t.Fatalf("dictionary-body write generated no mutation: %v", scen[0].GoldParams)
	}
	if !Grade(scen[0], Proposal{Tool: "t", Action: "set_meta", Params: scen[0].GoldParams}, idx).Pass() {
		t.Fatal("dictionary-body gold did not pass its own grading")
	}
}

// TestOpenBodyEnforcesRequiredWhenDynamicFieldSent pins that a proposal
// supplying only an additional field on an open body still counts as sending a
// body, so the body's RequiredWithBody properties are enforced.
func TestOpenBodyEnforcesRequiredWhenDynamicFieldSent(t *testing.T) {
	spec := ActionSpec{Tool: "t", Action: "put", Summary: "Put", BodyDynamic: true,
		Params: []ParamSpec{{Name: "kind", In: "body", RequiredWithBody: true, Type: "string"}}}
	if ok, _ := validateParams(spec, map[string]any{"extra": "x"}); ok {
		t.Fatal("an open-body proposal missing its required property was accepted")
	}
	if ok, r := validateParams(spec, map[string]any{}); !ok {
		t.Fatalf("no body should not require the property: %v", r)
	}
	if ok, r := validateParams(spec, map[string]any{"kind": "k", "extra": "y"}); !ok {
		t.Fatalf("a complete open-body proposal was rejected: %v", r)
	}
}

// TestCatalogAdvertisesDynamicBody pins that the model catalog tells the model
// an open-body action accepts additional fields, and that the instruction no
// longer forbids them unconditionally.
func TestCatalogAdvertisesDynamicBody(t *testing.T) {
	out := BuildSystem([]ActionSpec{{Tool: "t", Action: "set_meta", Summary: "Set meta", BodyDynamic: true,
		Params: []ParamSpec{{Name: "id", In: "path", Required: true, Type: "integer"}}}})
	if !strings.Contains(out, "additional body fields allowed") {
		t.Fatalf("catalog does not advertise the open body:\n%s", out)
	}
	if !strings.Contains(out, "unless it notes additional body fields") {
		t.Fatalf("instruction still forbids undeclared params unconditionally:\n%s", out)
	}
}

// TestPinnedCorpusGatesAnnotationDrift covers the gap grading cannot see: the
// oracle answers a pinned corpus with the pinned gold, so an action that only
// loses a safety annotation still scores 1 and the smoke stays green. The
// corpus preflight (checkCorpus) must reject the drift instead — this is the
// smoke-shaped end-to-end check over the whole fake catalog, on top of the
// per-shape cases in TestRunRejectsSafetyDriftBeforeSpend.
func TestPinnedCorpusGatesAnnotationDrift(t *testing.T) {
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
	pinned := Generate(specs, GenerateOptions{N: 100, Seed: 1})
	run := func(scenarios []Scenario) error {
		_, err := Run(ctx, session, Config{Models: []Model{NewOracleModel(scenarios)}, Scenarios: scenarios})
		return err
	}

	// The corpus as pinned matches the live catalog.
	if err := run(pinned); err != nil {
		t.Fatalf("matching corpus rejected: %v", err)
	}

	// An idempotent write that loses its Idempotent annotation grades
	// identically — same gold, same score — so only the drift check sees it.
	for _, want := range []Class{ClassIdempotent, ClassRead, ClassDestructive} {
		drifted := append([]Scenario(nil), pinned...)
		idx := -1
		for i, sc := range drifted {
			if sc.Class == want {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("fake catalog has no %s action to drift", want)
		}
		// Pin a class the live catalog no longer reports for that action.
		drifted[idx].Class = ClassWrite
		if want == ClassWrite {
			drifted[idx].Class = ClassRead
		}
		if err := run(drifted); err == nil {
			t.Fatalf("%s -> write annotation drift on %s was not gated", want, drifted[idx].ID)
		} else if !strings.Contains(err.Error(), drifted[idx].ID) {
			t.Fatalf("drift error does not name the scenario: %v", err)
		}
	}

	// A read action that stops being read-only drifts readonly_framed too.
	drifted := append([]Scenario(nil), pinned...)
	for i, sc := range drifted {
		if sc.ReadOnlyFramed {
			drifted[i].ReadOnlyFramed = false
			drifted[i].Class = ClassRead // isolate the readonly_framed check
			break
		}
	}
	if err := run(drifted); err == nil {
		t.Fatal("readonly_framed drift was not gated")
	}

	// A scenario whose gold action no longer exists is a corpus/surface
	// mismatch, refused by the corpus preflight before any cell runs — it is
	// not a drift finding, and never reaches scoring.
	gone := append([]Scenario(nil), pinned...)
	gone[0].GoldAction = "no_such_action"
	err = run(gone)
	if err == nil {
		t.Fatal("removed gold action was not refused")
	}
	if strings.Contains(err.Error(), "drifted") || !strings.Contains(err.Error(), "not in the live catalog") {
		t.Fatalf("removed action must be refused as a corpus mismatch, not reported as drift: %v", err)
	}
}

// TestCommittedCorporaLoad loads every corpus in testdata through the real
// loader. A hand edit to a pinned corpus that breaks its JSON would otherwise
// surface only when someone ran the documented product invocation, since the
// smoke pins only the fake corpus.
func TestCommittedCorporaLoad(t *testing.T) {
	paths, err := filepath.Glob("testdata/scenarios/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no committed corpora found")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := LoadCorpus(data)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// Every framing must be what generation renders for its gold params:
		// structured values as JSON, never Go's map[]/[value] spelling.
		for _, sc := range c.Scenarios {
			if strings.Contains(sc.NLFraming, "map[") {
				t.Fatalf("%s: %s framing carries Go rendering: %q", path, sc.ID, sc.NLFraming)
			}
		}
	}
}
