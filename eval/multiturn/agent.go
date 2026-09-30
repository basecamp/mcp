package multiturn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Agent drives one episode to completion: it reads ep.System, ep.Task.Prompt
// and ep.Surface.Tools, makes calls through ep.Call, and ends with ep.Finish.
type Agent interface {
	// Label is the report and baseline key, e.g. "haiku".
	Label() string
	// ModelID is the wire model behind the label, recorded so a baseline is
	// only ever compared like-for-like.
	ModelID() string
	Run(ctx context.Context, ep *Episode) error
}

// ScriptAgent plays each task's gold script: no model, no spend. It is the
// CI backend — it proves the loop turns end to end (connect, arm, calls,
// replay, grading, report) — and the corpus check: a task whose gold script
// fails against its cassettes is a broken task, not a hard one.
type ScriptAgent struct{}

func (ScriptAgent) Label() string   { return "script" }
func (ScriptAgent) ModelID() string { return "script" }

func (ScriptAgent) Run(ctx context.Context, ep *Episode) error {
	ep.Turns = 1
	// The canned answer is only earned by a clean gold path: a gold call
	// that errors (an action renamed, a cassette endpoint gone) fails the
	// episode, or answer-graded tasks would pass on the canned text alone.
	var failed []string
	for i, s := range ep.Task.Script {
		if text, isErr := ep.Call(ctx, s.Tool, s.Arguments); isErr {
			failed = append(failed, fmt.Sprintf("step %d (%s): %s", i+1, s.Tool, truncate(text, 200)))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("gold script failed: %s", strings.Join(failed, "; "))
	}
	ep.Finish(ep.Task.ScriptAnswer)
	return nil
}

// APIAgent is the Anthropic Messages API tool-use loop: the arm's system
// prompt, the arm's tools translated one-for-one from tools/list, and each
// tool_use answered with the MCP result until the model stops calling tools or
// the turn budget runs out. Usage is the API's exact count, cache included.
type APIAgent struct {
	label     string
	modelID   string
	apiKey    string
	endpoint  string
	maxTokens int
	client    *http.Client
}

// NewAPIAgent builds an API-backed agent. Requires ANTHROPIC_API_KEY.
func NewAPIAgent(label, modelID string) (*APIAgent, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set")
	}
	endpoint := os.Getenv("ANTHROPIC_BASE_URL")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	return &APIAgent{
		label: label, modelID: modelID, apiKey: key,
		endpoint:  strings.TrimSuffix(endpoint, "/") + "/v1/messages",
		maxTokens: 16000,
		client:    &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

func (a *APIAgent) Label() string   { return a.label }
func (a *APIAgent) ModelID() string { return a.modelID }

type apiBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type apiResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      Usage             `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *APIAgent) Run(ctx context.Context, ep *Episode) error {
	tools := make([]map[string]any, 0, len(ep.Surface.Tools))
	for _, t := range ep.Surface.Tools {
		tools = append(tools, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.InputSchema,
		})
	}
	messages := []map[string]any{{"role": "user", "content": ep.Task.Prompt}}
	// The answer is the last turn's text — or, when a model says its piece
	// alongside its final tool call and ends on an empty turn, the last text
	// it wrote.
	var lastText string

	for ep.Turns < ep.MaxTurns {
		resp, err := a.post(ctx, map[string]any{
			"model":      a.modelID,
			"max_tokens": a.maxTokens,
			"system":     ep.System,
			"tools":      tools,
			"messages":   messages,
			// Top-level automatic caching: the system prompt and tool list are
			// identical on every turn of an episode (and across episodes of an
			// arm), so each turn re-reads them from cache.
			"cache_control": map[string]any{"type": "ephemeral"},
		})
		if err != nil {
			return err
		}
		ep.Turns++
		ep.Usage.add(resp.Usage)
		// The assistant turn goes back verbatim — thinking blocks included,
		// which the API requires unchanged on the next request.
		messages = append(messages, map[string]any{"role": "assistant", "content": resp.Content})

		var text strings.Builder
		var results []map[string]any
		for _, raw := range resp.Content {
			var b apiBlock
			if err := json.Unmarshal(raw, &b); err != nil {
				return fmt.Errorf("decode content block: %w", err)
			}
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				var args map[string]any
				if err := json.Unmarshal(b.Input, &args); err != nil || args == nil {
					args = map[string]any{}
				}
				out, isErr := ep.Call(ctx, b.Name, args)
				results = append(results, map[string]any{
					"type": "tool_result", "tool_use_id": b.ID,
					"content": out, "is_error": isErr,
				})
			}
		}
		if strings.TrimSpace(text.String()) != "" {
			lastText = text.String()
		}
		if len(results) == 0 {
			// Only a turn the model ended itself is an answer; one cut off by
			// max_tokens (or stopped for any other reason) is not.
			if resp.StopReason != "end_turn" && resp.StopReason != "stop_sequence" {
				return fmt.Errorf("model stopped without finishing (stop_reason %q)", resp.StopReason)
			}
			ep.Finish(lastText)
			return nil
		}
		// Every result from one assistant turn goes back in one user message.
		messages = append(messages, map[string]any{"role": "user", "content": results})
	}
	ep.Exhausted = true
	return nil
}

func (a *APIAgent) post(ctx context.Context, body map[string]any) (*apiResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			last = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		// Retry what the API says is transient; fail fast on the rest.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			last = fmt.Errorf("anthropic api: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
			continue
		}
		var out apiResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("anthropic api: HTTP %d: decode: %w", resp.StatusCode, err)
		}
		if out.Error != nil {
			return nil, fmt.Errorf("anthropic api: %s: %s", out.Error.Type, out.Error.Message)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("anthropic api: HTTP %d", resp.StatusCode)
		}
		if out.StopReason == "refusal" {
			return nil, fmt.Errorf("anthropic api: the model refused")
		}
		return &out, nil
	}
	return nil, last
}
