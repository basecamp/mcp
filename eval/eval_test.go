package eval

import (
	"context"
	"reflect"
	"strings"
	"testing"
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
				{Name: "status", In: "body", Required: false, Type: "string", Enum: []string{"published", "drafted"}}}},
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
