package multiturn

// Pricing is a model's list price in USD per million tokens. Cache writes
// (5-minute TTL) bill at 1.25x input and cache reads at 0.1x.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Cost prices usage, cache traffic included.
func (p Pricing) Cost(u Usage) float64 {
	return (float64(u.InputTokens)*p.InputPerMTok +
		float64(u.CacheWriteTokens)*p.InputPerMTok*1.25 +
		float64(u.CacheReadTokens)*p.InputPerMTok*0.10 +
		float64(u.OutputTokens)*p.OutputPerMTok) / 1e6
}

// Models maps the labels the multi-turn mode knows to wire model ids: a small
// model and two frontier ones. --models takes these labels or any raw model
// id (which is then its own label).
var Models = map[string]string{
	"haiku":  "claude-haiku-4-5-20251001",
	"sonnet": "claude-sonnet-5",
	"opus":   "claude-opus-5-5",
}

// pricing is keyed by wire model id — a label can be retargeted, a price
// belongs to a model. Published Anthropic list prices.
var pricing = map[string]Pricing{
	"script":                    {},
	"claude-haiku-4-5-20251001": {InputPerMTok: 1.00, OutputPerMTok: 5.00},
	"claude-haiku-4-5":          {InputPerMTok: 1.00, OutputPerMTok: 5.00},
	"claude-sonnet-5":           {InputPerMTok: 2.00, OutputPerMTok: 10.00},
	"claude-opus-5-5":           {InputPerMTok: 4.00, OutputPerMTok: 20.00},
	"claude-opus-5":             {InputPerMTok: 5.00, OutputPerMTok: 25.00},
}

// ModelID resolves a --models label to its wire id; an unknown label is taken
// as a raw id.
func ModelID(label string) string {
	if id, ok := Models[label]; ok {
		return id
	}
	return label
}

// costOf prices usage for a wire model id. An id with no published rate is
// priced at the most expensive tier listed and marked estimated, so an ad-hoc
// model neither reads as free nor as cheap — and never passes for measured.
func costOf(modelID string, u Usage) (float64, bool) {
	if p, ok := pricing[modelID]; ok {
		return p.Cost(u), false
	}
	var max Pricing
	for _, p := range pricing {
		if p.InputPerMTok > max.InputPerMTok {
			max = p
		}
	}
	return max.Cost(u), true
}
