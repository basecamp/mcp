package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteJSONL appends one JSON object per record to w — the append-only results
// log.
func WriteJSONL(w io.Writer, records []Record) error {
	enc := json.NewEncoder(w)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// modelTotals is one model's aggregate over the corpus.
type modelTotals struct {
	label     string
	pass      int
	paramsOK  int
	safetyOK  int
	total     int
	inTokens  int
	outTokens int
	cost      float64
	errored   int
}

// Render renders the scored scenario×model table and the cost totals.
func (rep *Report) Render(server string) string {
	models := rep.modelOrder()
	byCell := map[string]Record{} // scenarioID|model -> record
	totals := map[string]*modelTotals{}
	for _, m := range models {
		totals[m] = &modelTotals{label: m}
	}
	for _, r := range rep.Records {
		byCell[r.ScenarioID+"|"+r.Model] = r
		t := totals[r.Model]
		t.total++
		if r.Score >= 1 {
			t.pass++
		}
		if r.ParamsMatch {
			t.paramsOK++
		}
		if r.AnnotationRespected {
			t.safetyOK++
		}
		if r.Error != "" {
			t.errored++
		}
		t.inTokens += r.InTokens
		t.outTokens += r.OutTokens
		t.cost += r.CostUSD
	}

	scenarios := append([]Scenario(nil), rep.Scenarios...)
	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].ID < scenarios[j].ID })

	var b strings.Builder
	fmt.Fprintf(&b, "MCP structural eval — server=%s  scenarios=%d  models=%s\n\n",
		server, len(scenarios), strings.Join(models, ","))

	idWidth := len("SCENARIO")
	for _, s := range scenarios {
		if len(s.ID) > idWidth {
			idWidth = len(s.ID)
		}
	}
	classWidth := len("CLASS")
	for _, s := range scenarios {
		if len(string(s.Class)) > classWidth {
			classWidth = len(string(s.Class))
		}
	}

	fmt.Fprintf(&b, "%-*s  %-*s", idWidth, "SCENARIO", classWidth, "CLASS")
	for _, m := range models {
		fmt.Fprintf(&b, "  %-10s", m)
	}
	b.WriteString("\n")
	for _, s := range scenarios {
		fmt.Fprintf(&b, "%-*s  %-*s", idWidth, s.ID, classWidth, string(s.Class))
		for _, m := range models {
			fmt.Fprintf(&b, "  %-10s", cell(byCell[s.ID+"|"+m]))
		}
		b.WriteString("\n")
	}

	b.WriteString("\nTOTALS\n")
	fmt.Fprintf(&b, "%-10s  %-8s  %-8s  %-8s  %-9s  %-9s  %-10s\n",
		"model", "pass", "params", "safety", "in_tok", "out_tok", "cost_usd")
	var grand float64
	for _, m := range models {
		t := totals[m]
		fmt.Fprintf(&b, "%-10s  %-8s  %-8s  %-8s  %-9d  %-9d  $%-9.4f\n",
			t.label,
			fmt.Sprintf("%d/%d", t.pass, t.total),
			fmt.Sprintf("%d/%d", t.paramsOK, t.total),
			fmt.Sprintf("%d/%d", t.safetyOK, t.total),
			t.inTokens, t.outTokens, t.cost)
		grand += t.cost
	}
	fmt.Fprintf(&b, "\nTOTAL COST: $%.4f over %d model-scenario calls\n", grand, len(rep.Records))
	return b.String()
}

// cell renders one table cell: PASS/FAIL, with a trailing ! on a safety
// violation and ? on a model/parse error.
func cell(r Record) string {
	if r.Error != "" {
		return "ERR?"
	}
	s := "FAIL"
	if r.Score >= 1 {
		s = "PASS"
	}
	if !r.AnnotationRespected {
		s += "!" // safety violation: destructive on a read-only-framed task
	}
	return s
}

// modelOrder returns the models in first-seen record order.
func (rep *Report) modelOrder() []string {
	var order []string
	seen := map[string]bool{}
	for _, r := range rep.Records {
		if !seen[r.Model] {
			seen[r.Model] = true
			order = append(order, r.Model)
		}
	}
	return order
}
