package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	// PricingEstimated marks a cost computed at a substituted price because
	// the model label carries no published rate. The figure is a plausible
	// floor, not a measured spend, and the report labels it as such.
	PricingEstimated bool   `json:"pricing_estimated,omitempty"`
	Error            string `json:"error,omitempty"`
	// ModelID is the resolved wire model the backend actually called — the
	// versioned API id, the CLI alias, or "oracle" — recorded alongside the
	// label so results from different underlying models never collapse into
	// one "haiku" when a rolling alias is retargeted.
	ModelID string `json:"model_id,omitempty"`
	// UsageEstimated marks token counts the backend did not report, filled
	// from the deterministic characters-per-token estimate (the CLI hides its
	// counts behind caching). The cost figure built on them is then an
	// estimate even at a published price, and the report says so.
	UsageEstimated bool `json:"usage_estimated,omitempty"`
	// Reasons explains a failed cell — the grader's own findings (missing
	// requested value, unknown param, enum violation) — so a results file can
	// be diagnosed without re-running the paid call. Empty on a pass.
	Reasons []string `json:"reasons,omitempty"`
	// ChoseParams is the proposal's params as the model sent them, kept for
	// the same reason: a failing record must show what was actually proposed.
	ChoseParams map[string]any `json:"chose_params,omitempty"`
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
	if err := checkModels(cfg.Models); err != nil {
		return nil, err
	}
	specs, err := SpecFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	idx := Index(specs)
	scenarios := cfg.Scenarios
	if scenarios == nil {
		scenarios = Generate(specs, cfg.Gen)
	} else if err := checkAnnotationDrift(scenarios, idx); err != nil {
		return nil, err
	}
	// An empty corpus grades nothing, and a run of zero cells must never read
	// as green. LoadCorpus already refuses an empty cached corpus; hold the same
	// line here so a live catalog that exposes no actions (or a caller-supplied
	// empty slice) fails loudly instead of rendering a zero-cell report.
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("no scenarios to run: the server catalog exposes no actions, or the corpus is empty")
	}
	if err := checkCorpus(scenarios, idx); err != nil {
		return nil, err
	}
	system := BuildSystem(specs)

	rep := &Report{Specs: specs, Scenarios: scenarios}
	for _, model := range cfg.Models {
		for _, sc := range scenarios {
			rep.Records = append(rep.Records, grade1(ctx, model, system, sc, idx))
		}
	}
	return rep, nil
}

// checkModels refuses a run with no models or with two models sharing a label.
// Records and the report's cells identify a model by its label alone, so a
// duplicate (--models haiku,haiku) would overwrite one run's
// cells with the other's while the totals still counted and charged both.
func checkModels(models []Model) error {
	if len(models) == 0 {
		return fmt.Errorf("no models to run")
	}
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m.Label()] {
			return fmt.Errorf("duplicate model label %q: each model in a run needs a distinct label", m.Label())
		}
		seen[m.Label()] = true
	}
	return nil
}

// checkCorpus proves the corpus can be graded against the live catalog before
// a single model call is made. Each failure below would otherwise hide behind
// a green-looking or misattributed run. A duplicate scenario ID: records and
// report cells key on the ID, so the second scenario overwrites the first's
// cell while the totals count and charge both. A duplicate framing: the oracle
// keys on it and a real model receives identical prompts with conflicting
// expected answers, so one of the two cells can never pass. An obsolete gold —
// a corpus written for a wider surface (fizzy with --writes), a narrowed domain
// set, or a catalog that renamed an action or added a required param — can
// never be satisfied, so every model burns the cell and the miss is reported
// as a model failure. Drifted safety metadata — a pinned read-only framing
// whose action is no longer read-only, or the reverse — makes the exact gold
// fail the safety dimension, or inflates the safety rate, after spend. All
// are corpus/surface mismatches, and they fail here, before spend.
func checkCorpus(scenarios []Scenario, idx SpecIndex) error {
	seenID := map[string]bool{}
	seenFraming := map[string]string{}
	for i, sc := range scenarios {
		if strings.TrimSpace(sc.ID) == "" {
			return fmt.Errorf("scenario #%d has no id: records and report cells key on it", i+1)
		}
		if strings.TrimSpace(sc.NLFraming) == "" {
			return fmt.Errorf("scenario %s has an empty framing: the model would be asked nothing, and the oracle would still answer it", sc.ID)
		}
		if seenID[sc.ID] {
			return fmt.Errorf("duplicate scenario id %q: records and report cells key on the id, so each scenario needs its own", sc.ID)
		}
		seenID[sc.ID] = true
		if prior, dup := seenFraming[sc.NLFraming]; dup {
			return fmt.Errorf("scenarios %s and %s share a framing %q: one request cannot have two expected answers", prior, sc.ID, sc.NLFraming)
		}
		seenFraming[sc.NLFraming] = sc.ID
		spec, ok := idx.lookup(sc.GoldTool, sc.GoldAction)
		if !ok {
			return fmt.Errorf("scenario %s: gold action %s/%s is not in the live catalog (corpus written for a different surface?)", sc.ID, sc.GoldTool, sc.GoldAction)
		}
		if ok, reasons := validateParams(spec, sc.GoldParams); !ok {
			return fmt.Errorf("scenario %s: gold params no longer valid against the live catalog: %v", sc.ID, reasons)
		}
		if got := classOf(spec); got != sc.Class {
			return fmt.Errorf("scenario %s: pinned class %q but the live catalog says %q (annotations drifted; regenerate the corpus)", sc.ID, sc.Class, got)
		}
		if spec.ReadOnly != sc.ReadOnlyFramed {
			return fmt.Errorf("scenario %s: pinned readonly_framed=%v but the live action readonly=%v (annotations drifted; regenerate the corpus)", sc.ID, sc.ReadOnlyFramed, spec.ReadOnly)
		}
	}
	return nil
}

// checkAnnotationDrift compares a pinned corpus's safety metadata against the
// live catalog. Grading cannot see this on its own: the oracle returns the
// pinned gold, the record's class comes from the pinned scenario, and an action
// that merely loses its Idempotent annotation still scores 1 — so a safety
// regression in the catalog passes the smoke. A renamed or removed action is
// left alone: it already scores 0 and gates as a newly-failing cell. Class is a
// lossy projection — ReadOnly wins, so a read action's Idempotent flag is not
// observed here; pinning the raw annotations is a corpus schema change that
// belongs with the per-record SHA fields.
func checkAnnotationDrift(scenarios []Scenario, idx SpecIndex) error {
	var drift []string
	for _, sc := range scenarios {
		spec, ok := idx.lookup(sc.GoldTool, sc.GoldAction)
		if !ok {
			continue
		}
		if got := classOf(spec); got != sc.Class {
			drift = append(drift, fmt.Sprintf("%s: pinned class %q, catalog now %q", sc.ID, sc.Class, got))
		}
		if spec.ReadOnly != sc.ReadOnlyFramed {
			drift = append(drift, fmt.Sprintf("%s: pinned readonly_framed=%v, catalog readonly=%v", sc.ID, sc.ReadOnlyFramed, spec.ReadOnly))
		}
	}
	if len(drift) > 0 {
		return fmt.Errorf("catalog annotations drifted from the pinned corpus:\n  %s", strings.Join(drift, "\n  "))
	}
	return nil
}

// grade1 runs and grades a single (model, scenario) cell.
func grade1(ctx context.Context, model Model, system string, sc Scenario, idx SpecIndex) Record {
	user := BuildUser(sc)
	rec := Record{
		Model:          model.Label(),
		ModelID:        model.ModelID(),
		ScenarioID:     sc.ID,
		Class:          sc.Class,
		GoldTool:       sc.GoldTool,
		ExpectedAction: sc.GoldAction,
		// Default to safe: a call that errors or fails to parse proposes no
		// action, so it is not a safety violation and must not drag the safety
		// rate down. Grade overrides this on a real destructive misfire.
		AnnotationRespected: true,
	}

	text, usage, err := model.Propose(ctx, system, user)
	if err != nil {
		// A call that never reached a model is not a paid call: record only
		// usage the backend explicitly returned, never a prompt-size estimate.
		rec.Error = err.Error()
		rec.InTokens = usage.InputTokens
		rec.OutTokens = usage.OutputTokens
		rec.CostUSD, rec.PricingEstimated = costOf(model.Label(), usage)
		return rec
	}

	// On success, fall back to the deterministic estimate for whichever counts
	// the backend did not report (the CLI hides most input tokens behind cache),
	// and say so: a cost built on estimated counts is an estimate even at a
	// published price.
	if usage.InputTokens == 0 {
		usage.InputTokens = EstimateTokens(system) + EstimateTokens(user)
		rec.UsageEstimated = true
	}
	if usage.OutputTokens == 0 {
		usage.OutputTokens = EstimateTokens(text)
		rec.UsageEstimated = true
	}
	rec.InTokens = usage.InputTokens
	rec.OutTokens = usage.OutputTokens
	rec.CostUSD, rec.PricingEstimated = costOf(model.Label(), usage)

	prop, perr := ParseProposal(text)
	if perr != nil {
		rec.Error = perr.Error()
		rec.ChoseTool = ""
		return rec
	}
	rec.ChoseTool = prop.Tool
	rec.ChoseAction = prop.Action
	rec.ChoseParams = prop.Params

	res := Grade(sc, prop, idx)
	rec.ToolMatch = res.ToolMatch
	rec.ActionMatch = res.ActionMatch
	rec.ParamsMatch = res.ParamsValid
	rec.AnnotationRespected = res.AnnotationRespected
	rec.Score = res.Score
	if !res.Pass() {
		rec.Reasons = res.Reasons
	}
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

func (m *OracleModel) Label() string   { return m.label }
func (m *OracleModel) ModelID() string { return m.label }

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

// Corpus is a cached scenario corpus with the metadata needed to validate it
// against the run it is loaded into — above all the server it was generated for,
// so a Fizzy corpus is never graded against a different live catalog.
type Corpus struct {
	Server    string
	Seed      int64
	N         int
	Scenarios []Scenario
}

// LoadCorpus reads a cached corpus and its metadata.
func LoadCorpus(data []byte) (Corpus, error) {
	var sj scenariosJSON
	if err := json.Unmarshal(data, &sj); err != nil {
		return Corpus{}, fmt.Errorf("decode scenarios: %w", err)
	}
	// A corpus with no scenarios is not an empty experiment, it is a broken
	// one: an empty slice is non-nil, so it suppresses generation, the run
	// grades nothing, and --require-pass passes vacuously because there are no
	// failing records. Fail closed rather than reporting a green run of zero
	// cells.
	if len(sj.Scenarios) == 0 {
		return Corpus{}, fmt.Errorf("decode scenarios: corpus contains no scenarios")
	}
	return Corpus{Server: sj.Server, Seed: sj.Seed, N: sj.N, Scenarios: sj.Scenarios}, nil
}

// UnmarshalScenarios reads just the scenarios from a cached corpus. Prefer
// LoadCorpus when the server metadata matters.
func UnmarshalScenarios(data []byte) ([]Scenario, error) {
	c, err := LoadCorpus(data)
	if err != nil {
		return nil, err
	}
	return c.Scenarios, nil
}
