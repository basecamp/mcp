package multiturn

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteJSONL writes one record per line.
func WriteJSONL(w io.Writer, records []Record) error {
	enc := json.NewEncoder(w)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// RequireMeasured fails a run in which every episode errored (a bad key, an
// outage, a server that dies after preflight): its results file holds no
// measurement at all. A run with any measured episode reports its errors
// apart instead.
func RequireMeasured(records []Record) error {
	for _, r := range records {
		if r.Error == "" {
			return nil
		}
	}
	if len(records) == 0 {
		return fmt.Errorf("the run produced no records")
	}
	return fmt.Errorf("no episode was measured: all %d errored (first: %s/%s/%s: %s)", len(records), records[0].Model, records[0].Arm, records[0].TaskID, records[0].Error)
}

// RequirePass errors unless there are records and every one passed — the gate
// for the deterministic script smoke, where a failure is a broken corpus,
// cassette, or harness, never a model.
func RequirePass(records []Record) error {
	if len(records) == 0 {
		return fmt.Errorf("the run produced no records")
	}
	var failing []string
	for _, r := range records {
		if !r.Pass || r.Error != "" {
			failing = append(failing, r.Model+"/"+r.Arm+"/"+r.TaskID)
		}
	}
	if len(failing) > 0 {
		return fmt.Errorf("%d of %d episodes did not pass (first: %s)", len(failing), len(records), failing[0])
	}
	return nil
}

// cellTotals aggregates one (model, arm) column.
type cellTotals struct {
	model, arm             string
	n, pass, errored       int
	calls, turns, guide    int
	wrongID, wrongTool     int
	safety, safetyEpisodes int
	in, out, cw, cr        int
	cost                   float64
	estimated              bool
}

// Render prints the task grid and the model × arm summary.
func (rep *Report) Render() string {
	type key struct{ model, arm string }
	var order []key
	totals := map[key]*cellTotals{}
	byCell := map[string]Record{}
	for _, r := range rep.Records {
		k := key{r.Model, r.Arm}
		t, ok := totals[k]
		if !ok {
			t = &cellTotals{model: r.Model, arm: r.Arm}
			totals[k] = t
			order = append(order, k)
		}
		t.n++
		// Spend is real either way; everything else only for a measured
		// episode — an errored one (API, CLI, transport) measured nothing,
		// and would otherwise dilute the averages and the safety ratio.
		t.in += r.InTokens
		t.out += r.OutTokens
		t.cw += r.CacheWriteTokens
		t.cr += r.CacheReadTokens
		t.cost += r.CostUSD
		t.estimated = t.estimated || r.PricingEstimated
		byCell[r.TaskID+"|"+r.Model+"|"+r.Arm] = r
		if r.Error != "" {
			t.errored++
			continue
		}
		if r.Pass {
			t.pass++
		}
		t.calls += r.Calls
		t.turns += r.Turns
		t.guide += r.GuideCalls
		t.wrongID += r.WrongID
		t.wrongTool += r.WrongTool
		t.safety += r.Safety
		if r.Safety > 0 {
			t.safetyEpisodes++
		}
	}

	var b strings.Builder
	var models, arms []string
	seenM, seenA := map[string]bool{}, map[string]bool{}
	for _, k := range order {
		if !seenM[k.model] {
			seenM[k.model] = true
			models = append(models, k.model)
		}
		if !seenA[k.arm] {
			seenA[k.arm] = true
			arms = append(arms, k.arm)
		}
	}
	fmt.Fprintf(&b, "MCP multi-turn eval — server=%s  tasks=%d  models=%s  arms=%s\n\n",
		rep.Server, len(rep.Tasks), strings.Join(models, ","), strings.Join(arms, ","))

	// Task grid.
	idW := len("TASK")
	for _, t := range rep.Tasks {
		if len(t.ID) > idW {
			idW = len(t.ID)
		}
	}
	colW := 6
	for _, k := range order {
		if w := len(k.model) + 1 + len(k.arm); w > colW {
			colW = w
		}
	}
	fmt.Fprintf(&b, "%-*s", idW, "TASK")
	for _, k := range order {
		fmt.Fprintf(&b, "  %-*s", colW, k.model+"/"+k.arm)
	}
	b.WriteString("\n")
	for _, t := range rep.Tasks {
		fmt.Fprintf(&b, "%-*s", idW, t.ID)
		for _, k := range order {
			fmt.Fprintf(&b, "  %-*s", colW, gridCell(byCell[t.ID+"|"+k.model+"|"+k.arm]))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nPASS  FAIL  ERR  (! safety violation, ~ turn budget exhausted)\n")

	// Summary.
	b.WriteString("\nSUMMARY (model × arm)\n")
	fmt.Fprintf(&b, "%-8s  %-14s  %-7s  %-5s  %-4s  %-10s  %-10s  %-6s  %-8s  %-10s  %-6s  %-9s  %-9s  %-9s  %s\n",
		"model", "arm", "pass", "rate", "err", "calls/task", "turns/task", "guide", "wrong_id", "wrong_tool", "safety", "in_tok", "out_tok", "cache_rd", "cost_usd")
	var grand float64
	anyEst := false
	for _, k := range order {
		t := totals[k]
		measured := t.n - t.errored
		per := func(x int) float64 {
			if measured == 0 {
				return 0
			}
			return float64(x) / float64(measured)
		}
		fmt.Fprintf(&b, "%-8s  %-14s  %-7s  %-5s  %-4d  %-10.1f  %-10.1f  %-6d  %-8d  %-10d  %-6s  %-9d  %-9d  %-9d  %s\n",
			t.model, t.arm,
			fmt.Sprintf("%d/%d", t.pass, measured),
			fmt.Sprintf("%.0f%%", 100*per(t.pass)),
			t.errored,
			per(t.calls), per(t.turns),
			t.guide, t.wrongID, t.wrongTool,
			fmt.Sprintf("%d/%d", t.safetyEpisodes, measured),
			t.in+t.cw, t.out, t.cr, costFigure(t.cost, t.estimated))
		grand += t.cost
		anyEst = anyEst || t.estimated
	}
	fmt.Fprintf(&b, "\nTOTAL COST: %s over %d episodes\n", costFigure(grand, anyEst), len(rep.Records))
	b.WriteString("in_tok counts uncached input plus cache writes; cache_rd is cache reads (billed at 0.1x input).\n")
	b.WriteString("pass, rates and per-task figures count measured episodes; err episodes (API, CLI, or transport failures) are shown apart, their spend included.\n")
	b.WriteString("wrong_id: backend requests the cassette could not answer (404). wrong_tool: calls rejected before the backend. safety: episodes with a violation.\n")
	if anyEst {
		b.WriteString("(estimated) — a model id with no published rate was priced at the most expensive listed tier.\n")
	}
	return b.String()
}

func gridCell(r Record) string {
	if r.TaskID == "" {
		return "-"
	}
	s := "FAIL"
	switch {
	case r.Error != "":
		s = "ERR"
	case r.Pass:
		s = "PASS"
	}
	if r.Safety > 0 {
		s += "!"
	}
	if r.Exhausted {
		s += "~"
	}
	return s
}

func costFigure(cost float64, estimated bool) string {
	if estimated {
		return fmt.Sprintf("$%.4f (estimated)", cost)
	}
	return fmt.Sprintf("$%.4f", cost)
}
