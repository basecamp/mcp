package eval

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// Class buckets an action by its safety annotations. The four classes drive
// weighted sampling (destructive and idempotent actions are the ones an agent
// most needs to get right, so the corpus over-represents them) and the
// read-only safety check.
type Class string

const (
	ClassRead        Class = "read"        // read-only
	ClassIdempotent  Class = "idempotent"  // idempotent write
	ClassDestructive Class = "destructive" // deletes or irreversibly alters
	ClassWrite       Class = "write"       // non-idempotent, non-destructive write
)

// classOf buckets one action.
func classOf(a ActionSpec) Class {
	switch {
	case a.ReadOnly:
		return ClassRead
	case a.Destructive:
		return ClassDestructive
	case a.Idempotent:
		return ClassIdempotent
	default:
		return ClassWrite
	}
}

// classWeight over-samples the actions whose misuse is costly. Destructive and
// idempotent-write actions carry the sharpest safety and correctness signal,
// so they are weighted above plain reads and non-idempotent writes.
var classWeight = map[Class]int{
	ClassRead:        1,
	ClassWrite:       2,
	ClassIdempotent:  3,
	ClassDestructive: 4,
}

// Scenario is one graded task: a natural-language framing plus the gold
// {tool, action, params} it should resolve to. Gold params carry synthetic
// values that the framing names, so grading validates the model's params both
// against the catalog schema and against these requested values.
type Scenario struct {
	ID             string         `json:"scenario_id"`
	Class          Class          `json:"class"`
	NLFraming      string         `json:"nl_framing"`
	GoldTool       string         `json:"gold_tool"`
	GoldAction     string         `json:"gold_action"`
	GoldParams     map[string]any `json:"gold_params"`
	ReadOnlyFramed bool           `json:"readonly_framed"`
}

// GenerateOptions configures scenario generation.
type GenerateOptions struct {
	// N is how many scenarios to emit (capped at the number of distinct
	// actions available).
	N int
	// Seed makes generation reproducible: the same specs, N, and seed always
	// yield the same corpus.
	Seed int64
}

// Generate builds a deterministic, seedable scenario corpus from the action
// specs, sampling distinct actions weighted toward the destructive and
// idempotent classes. It is a pure function of (specs, opts): no clock, no
// global rand, no network.
func Generate(specs []ActionSpec, opts GenerateOptions) []Scenario {
	if opts.N <= 0 {
		opts.N = 12
	}
	// Copy and sort for a stable, input-order-independent starting point.
	pool := append([]ActionSpec(nil), specs...)
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].Tool != pool[j].Tool {
			return pool[i].Tool < pool[j].Tool
		}
		return pool[i].Action < pool[j].Action
	})

	rng := rand.New(rand.NewSource(opts.Seed))
	chosen := weightedSampleDistinct(rng, pool, opts.N)

	scenarios := make([]Scenario, 0, len(chosen))
	for _, spec := range chosen {
		scenarios = append(scenarios, buildScenario(rng, spec))
	}
	disambiguateFramings(scenarios)
	return scenarios
}

// disambiguateFramings guarantees every scenario's NL framing is unique, so a
// framing-keyed consumer (the oracle) can never map two distinct actions to one
// answer. Collisions are rare — two actions with identical summaries and params.
// The disambiguator is a neutral ordinal, never the gold action name: the
// catalog prompt exposes those names, so naming one in the request would hand
// the model the answer and inflate that cell.
func disambiguateFramings(scenarios []Scenario) {
	seen := map[string]bool{}
	for i := range scenarios {
		f := scenarios[i].NLFraming
		if !seen[f] {
			seen[f] = true
			continue
		}
		for n := 2; ; n++ {
			alt := fmt.Sprintf("%s (%d)", f, n)
			if !seen[alt] {
				scenarios[i].NLFraming = alt
				seen[alt] = true
				break
			}
		}
	}
}

// weightedSampleDistinct draws up to n distinct actions without replacement,
// each action's draw probability proportional to its class weight. Deterministic
// under rng.
func weightedSampleDistinct(rng *rand.Rand, pool []ActionSpec, n int) []ActionSpec {
	remaining := append([]ActionSpec(nil), pool...)
	if n > len(remaining) {
		n = len(remaining)
	}
	var out []ActionSpec
	for len(out) < n && len(remaining) > 0 {
		total := 0
		for _, s := range remaining {
			total += classWeight[classOf(s)]
		}
		target := rng.Intn(total)
		idx := 0
		for i, s := range remaining {
			target -= classWeight[classOf(s)]
			if target < 0 {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx])
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}

// buildScenario renders one scenario: synthetic gold params, a natural-language
// framing that names them, and the gold resolution.
func buildScenario(rng *rand.Rand, spec ActionSpec) Scenario {
	class := classOf(spec)
	gold := goldParams(rng, spec)
	return Scenario{
		ID:             spec.Tool + "." + spec.Action,
		Class:          class,
		NLFraming:      frame(class, spec, gold),
		GoldTool:       spec.Tool,
		GoldAction:     spec.Action,
		GoldParams:     gold,
		ReadOnlyFramed: spec.ReadOnly,
	}
}

// goldParams synthesizes a value for every required parameter. For a write
// action it also names at least one body field, so the task expresses the
// change to make (an update framed with only its ids is a no-op the model can
// "pass" without understanding). Finally it exercises an enum when the params
// so far include none.
func goldParams(rng *rand.Rand, spec ActionSpec) map[string]any {
	out := map[string]any{}
	for _, p := range spec.Params {
		if p.Required {
			out[p.Name] = syntheticValue(rng, p)
		}
	}

	// A write must carry a mutation: if no body field is set yet and the action
	// offers optional body fields, add one (preferring an enum field).
	if !spec.ReadOnly && !hasIn(out, spec, "body") {
		if p, ok := firstOptional(spec, "body"); ok {
			out[p.Name] = syntheticValue(rng, p)
		}
	}

	// Exercise an enum when nothing chosen so far constrains one.
	if !hasEnum(out, spec) {
		if p, ok := firstOptionalEnum(spec, out); ok {
			out[p.Name] = p.Enum[rng.Intn(len(p.Enum))]
		}
	}
	return out
}

// hasIn reports whether any chosen param sits in the given schema location.
func hasIn(chosen map[string]any, spec ActionSpec, in string) bool {
	for name := range chosen {
		if p, ok := spec.param(name); ok && p.In == in {
			return true
		}
	}
	return false
}

// hasEnum reports whether any chosen param is enum-constrained.
func hasEnum(chosen map[string]any, spec ActionSpec) bool {
	for name := range chosen {
		if p, ok := spec.param(name); ok && len(p.Enum) > 0 {
			return true
		}
	}
	return false
}

// firstOptional returns the first optional param in the given location, in
// declaration order, preferring one with an enum.
func firstOptional(spec ActionSpec, in string) (ParamSpec, bool) {
	var fallback ParamSpec
	found := false
	for _, p := range spec.Params {
		if p.Required || p.In != in {
			continue
		}
		if len(p.Enum) > 0 {
			return p, true
		}
		if !found {
			fallback, found = p, true
		}
	}
	return fallback, found
}

// firstOptionalEnum returns the first optional enum param not already chosen.
func firstOptionalEnum(spec ActionSpec, chosen map[string]any) (ParamSpec, bool) {
	for _, p := range spec.Params {
		if !p.Required && len(p.Enum) > 0 {
			if _, taken := chosen[p.Name]; !taken {
				return p, true
			}
		}
	}
	return ParamSpec{}, false
}

// syntheticValue produces a stable, type-appropriate gold value for one param.
func syntheticValue(rng *rand.Rand, p ParamSpec) any {
	if len(p.Enum) > 0 {
		return p.Enum[rng.Intn(len(p.Enum))]
	}
	idLike := strings.HasSuffix(p.Name, "_id") || strings.HasSuffix(p.Name, "_number")
	switch p.Type {
	case "integer", "number":
		return 100 + rng.Intn(900)
	case "boolean":
		return true
	case "array":
		return []any{niceString(p.Name)}
	case "object":
		// An empty object satisfies the structural type check; v0 does not
		// recurse into nested object schemas.
		return map[string]any{}
	case "string":
		// Respect the declared type: an id/number declared as a string (Fizzy
		// renders path tokens this way) needs a numeric string, not an int.
		if idLike {
			return strconv.Itoa(100 + rng.Intn(900))
		}
		return niceString(p.Name)
	default:
		// Untyped path tokens (ids, numbers) read as integers; named strings
		// get a readable phrase.
		if idLike {
			return 100 + rng.Intn(900)
		}
		return niceString(p.Name)
	}
}

// niceString maps a few common field names to readable values and derives the
// rest from the name, so framings read naturally.
func niceString(name string) string {
	switch name {
	case "name", "title":
		return "Q3 Roadmap"
	case "content", "body", "description", "public_description":
		return "Draft copy for review"
	case "email", "email_address":
		return "sam@example.com"
	case "color":
		return "blue"
	default:
		return capitalize(strings.ReplaceAll(name, "_", " "))
	}
}

// capitalize upper-cases the first letter, leaving the rest untouched.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// frame renders the natural-language task. Read-only actions are framed as
// questions or lookups; writes as requests. The gold param values are named so
// the model has the concrete ids it needs, but the action and tool are not.
func frame(class Class, spec ActionSpec, gold map[string]any) string {
	summary := strings.TrimSpace(spec.Summary)
	summary = strings.TrimSuffix(summary, ".")
	request := lowerFirst(summary)

	var opener string
	switch class {
	case ClassRead:
		opener = "Could you"
	case ClassDestructive:
		opener = "Please go ahead and"
	default:
		opener = "Please"
	}

	clause := paramClause(spec, gold)
	if clause != "" {
		return fmt.Sprintf("%s %s %s.", opener, request, clause)
	}
	return fmt.Sprintf("%s %s.", opener, request)
}

// paramClause renders the gold params as a readable trailing clause.
func paramClause(spec ActionSpec, gold map[string]any) string {
	if len(gold) == 0 {
		return ""
	}
	names := make([]string, 0, len(gold))
	for name := range gold {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %v", strings.ReplaceAll(name, "_", " "), quoteIfString(gold[name])))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func quoteIfString(v any) any {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return v
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
