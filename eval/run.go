package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Record is one graded (model, scenario) row, the unit appended to the JSONL
// results and totalled in the report.
type Record struct {
	Model               string  `json:"model"`
	ScenarioID          string  `json:"scenario_id"`
	Class               Class   `json:"class"`
	GoldTool            string  `json:"tool"`
	ExpectedAction      string  `json:"expected_action"`
	ChoseTool           string  `json:"chose_tool"`
	ChoseAction         string  `json:"chose_action"`
	ToolMatch           bool    `json:"tool_match"`
	ActionMatch         bool    `json:"action_match"`
	ParamsMatch         bool    `json:"params_match"`
	AnnotationRespected bool    `json:"annotation_respected"`
	Score               float64 `json:"score"`
	InTokens            int     `json:"in_tokens"`
	OutTokens           int     `json:"out_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	Error               string  `json:"error,omitempty"`
}

// Config parameterizes a run.
type Config struct {
	// Models are the model backends to evaluate, in report column order.
	Models []Model
	// Scenarios is the corpus. When nil, it is generated from the server's
	// spec using Gen.
	Scenarios []Scenario
	// Gen configures generation when Scenarios is nil.
	Gen GenerateOptions
}

// Report is a completed run: the derived spec, the scenarios, and every graded
// record, ready to render or serialize.
type Report struct {
	Specs     []ActionSpec
	Scenarios []Scenario
	Records   []Record
}

// Run derives the spec from the live server, resolves the scenario corpus,
// drives each model over every scenario, grades by rule, and returns the
// report. The server round-trip touches only list and describe, so no backend
// or credentials are needed.
func Run(ctx context.Context, session *mcp.ClientSession, cfg Config) (*Report, error) {
	specs, err := SpecFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	scenarios := cfg.Scenarios
	if scenarios == nil {
		scenarios = Generate(specs, cfg.Gen)
	}
	idx := Index(specs)
	system := BuildSystem(specs)

	rep := &Report{Specs: specs, Scenarios: scenarios}
	for _, model := range cfg.Models {
		for _, sc := range scenarios {
			rep.Records = append(rep.Records, grade1(ctx, model, system, sc, idx))
		}
	}
	return rep, nil
}

// grade1 runs and grades a single (model, scenario) cell.
func grade1(ctx context.Context, model Model, system string, sc Scenario, idx SpecIndex) Record {
	user := BuildUser(sc)
	rec := Record{
		Model:          model.Label(),
		ScenarioID:     sc.ID,
		Class:          sc.Class,
		GoldTool:       sc.GoldTool,
		ExpectedAction: sc.GoldAction,
	}

	text, usage, err := model.Propose(ctx, system, user)
	if usage.InputTokens == 0 {
		usage.InputTokens = EstimateTokens(system) + EstimateTokens(user)
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = EstimateTokens(text)
	}
	rec.InTokens = usage.InputTokens
	rec.OutTokens = usage.OutputTokens
	rec.CostUSD = PricingFor(model.Label()).Cost(usage)

	if err != nil {
		rec.Error = err.Error()
		return rec
	}

	prop, perr := ParseProposal(text)
	if perr != nil {
		rec.Error = perr.Error()
		rec.ChoseTool = ""
		return rec
	}
	rec.ChoseTool = prop.Tool
	rec.ChoseAction = prop.Action

	res := Grade(sc, prop, idx)
	rec.ToolMatch = res.ToolMatch
	rec.ActionMatch = res.ActionMatch
	rec.ParamsMatch = res.ParamsValid
	rec.AnnotationRespected = res.AnnotationRespected
	rec.Score = res.Score
	return rec
}

// OracleModel answers every scenario with its gold resolution. It is the
// deterministic, zero-spend backend for tests and the CI smoke run: it proves
// the loop turns — spec, generation, prompting, parsing, grading, reporting —
// without any model call.
type OracleModel struct {
	label string
	gold  map[string]Proposal // keyed by NL framing
}

// NewOracleModel builds an oracle that returns each scenario's gold answer.
func NewOracleModel(scenarios []Scenario) *OracleModel {
	gold := map[string]Proposal{}
	for _, s := range scenarios {
		gold[s.NLFraming] = Proposal{Tool: s.GoldTool, Action: s.GoldAction, Params: s.GoldParams}
	}
	return &OracleModel{label: "oracle", gold: gold}
}

func (m *OracleModel) Label() string { return m.label }

func (m *OracleModel) Propose(_ context.Context, _, user string) (string, Usage, error) {
	framing := user
	if len(user) > len("Request: ") && user[:len("Request: ")] == "Request: " {
		framing = user[len("Request: "):]
	}
	p, ok := m.gold[framing]
	if !ok {
		return `{"tool":"","action":"","params":{}}`, Usage{}, nil
	}
	data, _ := json.Marshal(p)
	return string(data), Usage{}, nil
}

// scenariosJSON is the on-disk cache shape for a scenario corpus.
type scenariosJSON struct {
	Server    string     `json:"server"`
	Seed      int64      `json:"seed"`
	N         int        `json:"n"`
	Scenarios []Scenario `json:"scenarios"`
}

// MarshalScenarios serializes a corpus for the testdata cache.
func MarshalScenarios(server string, gen GenerateOptions, scenarios []Scenario) ([]byte, error) {
	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].ID < scenarios[j].ID })
	return json.MarshalIndent(scenariosJSON{
		Server: server, Seed: gen.Seed, N: gen.N, Scenarios: scenarios,
	}, "", "  ")
}

// UnmarshalScenarios reads a cached corpus.
func UnmarshalScenarios(data []byte) ([]Scenario, error) {
	var sj scenariosJSON
	if err := json.Unmarshal(data, &sj); err != nil {
		return nil, fmt.Errorf("decode scenarios: %w", err)
	}
	return sj.Scenarios, nil
}
