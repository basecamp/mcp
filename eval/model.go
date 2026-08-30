package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Usage is the token cost of one model turn.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Pricing is a model's list price in USD per million tokens.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Cost returns the USD cost of the usage at this pricing.
func (p Pricing) Cost(u Usage) float64 {
	return float64(u.InputTokens)/1e6*p.InputPerMTok +
		float64(u.OutputTokens)/1e6*p.OutputPerMTok
}

// pricingTable holds published Anthropic list prices (USD per million tokens)
// for the cheap models an eval turn uses. Frugality is a first-class metric:
// the cost figure the report prints is computed from these, so it stays honest
// even when the model is reached through a gateway that hides billing.
var pricingTable = map[string]Pricing{
	"oracle": {InputPerMTok: 0, OutputPerMTok: 0},        // deterministic, no spend
	"haiku":  {InputPerMTok: 0.80, OutputPerMTok: 4.00},  // claude-3-5-haiku
	"sonnet": {InputPerMTok: 3.00, OutputPerMTok: 15.00}, // claude-sonnet
}

// PricingFor returns the pricing for a known model label. An unknown label has
// no price we can vouch for, so it returns false rather than fabricating one —
// the caller decides whether to fail or fall back. The deterministic oracle is
// priced at zero, keeping the zero-spend smoke honestly free.
func PricingFor(label string) (Pricing, bool) {
	p, ok := pricingTable[label]
	return p, ok
}

// costOf prices usage for a model label. An unknown label falls back to Haiku
// (the cheapest paid tier) so a custom model still reports a plausible,
// non-zero cost rather than appearing free.
func costOf(label string, u Usage) float64 {
	p, ok := PricingFor(label)
	if !ok {
		p = pricingTable["haiku"]
	}
	return p.Cost(u)
}

// EstimateTokens approximates a string's token count at ~4 characters per
// token. It is deterministic on purpose: the headline cost metric is a pure
// function of the prompt and the answer, so it is reproducible and costs
// nothing to compute. Backends that report exact usage override it.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// Model proposes a tool call for one scenario. Implementations differ only in
// where the answer comes from — a real API, the local claude CLI, or a
// deterministic oracle for tests.
type Model interface {
	// Label is the pricing/report key, e.g. "haiku".
	Label() string
	// Propose returns the model's raw text answer. A nonzero Usage is the
	// backend's own exact count; a zero Usage tells the runner to estimate.
	Propose(ctx context.Context, system, user string) (text string, usage Usage, err error)
}

// APIModel calls the Anthropic Messages API directly — the lean path a real
// eval runs, whose token usage is exact. Requires ANTHROPIC_API_KEY.
type APIModel struct {
	label     string
	modelID   string
	apiKey    string
	maxTokens int
	client    *http.Client
}

// NewAPIModel builds an API-backed model. label selects pricing; modelID is
// the wire model name.
func NewAPIModel(label, modelID string) (*APIModel, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set")
	}
	return &APIModel{
		label:     label,
		modelID:   modelID,
		apiKey:    key,
		maxTokens: 512,
		client:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (m *APIModel) Label() string { return m.label }

func (m *APIModel) Propose(ctx context.Context, system, user string) (string, Usage, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model":      m.modelID,
		"max_tokens": m.maxTokens,
		"system":     system,
		"messages":   []map[string]any{{"role": "user", "content": user}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("x-api-key", m.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", Usage{}, err
	}
	if out.Error != nil {
		return "", Usage{}, fmt.Errorf("anthropic api: %s", out.Error.Message)
	}
	var text strings.Builder
	for _, c := range out.Content {
		text.WriteString(c.Text)
	}
	return text.String(), Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}, nil
}

// CLIModel reaches a model through the local `claude` CLI in headless print
// mode. It needs no API key, so it can run in environments where the model is
// only reachable through the CLI gateway. Token usage is estimated (the CLI
// hides most input tokens behind prompt caching), so the runner's deterministic
// estimate governs cost.
type CLIModel struct {
	label   string
	modelID string
	bin     string
	timeout time.Duration
}

// NewCLIModel builds a CLI-backed model. modelID is passed to `claude --model`.
func NewCLIModel(label, modelID string) *CLIModel {
	bin := os.Getenv("EVAL_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	return &CLIModel{label: label, modelID: modelID, bin: bin, timeout: 120 * time.Second}
}

func (m *CLIModel) Label() string { return m.label }

func (m *CLIModel) Propose(ctx context.Context, system, user string) (string, Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	prompt := system + "\n\n" + user
	cmd := exec.CommandContext(ctx, m.bin, "-p", "--model", m.modelID, "--output-format", "json")
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", Usage{}, fmt.Errorf("claude cli: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Usage   struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return "", Usage{}, fmt.Errorf("claude cli: decode output: %w", err)
	}
	if out.IsError {
		return "", Usage{}, fmt.Errorf("claude cli reported error: %s", out.Result)
	}
	// The CLI's usage counts are unreliable under caching; return zero so the
	// runner estimates from the exact prompt and answer it controls.
	return out.Result, Usage{}, nil
}
