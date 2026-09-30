package multiturn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/eval/cassette"
)

// Backend is the replayed (or recorded) API the server under test talks to.
// The episode reads its exchange log to attribute backend requests to calls.
type Backend interface {
	Len() int
	Since(mark int) []cassette.Exchange
}

// Step is one tool call in a trace.
type Step struct {
	// Turn is the model turn that made the call; -1 when the host reports
	// turns only in aggregate (the claude CLI).
	Turn int    `json:"turn"`
	Tool string `json:"tool"`
	// Op is the surface-independent operation: a gateway call's action, or
	// the tool name for a flat tool. Params likewise: a gateway call's
	// params, or the whole argument object.
	Op     string         `json:"op"`
	Params map[string]any `json:"params,omitempty"`
	// IsError is the tool result's error flag; Unknown marks a call to a
	// tool the model was not shown.
	IsError bool `json:"is_error,omitempty"`
	Unknown bool `json:"unknown,omitempty"`
	// Result is the result text, truncated for the record.
	Result string `json:"result,omitempty"`
	// Requests are the backend exchanges this call produced.
	Requests []cassette.Exchange `json:"requests,omitempty"`
}

// Line renders the step as one matchable line: `<op> <params JSON>`, keys
// sorted (encoding/json sorts map keys), so a pattern written once matches a
// gateway call and a flat call alike.
func (s Step) Line() string {
	params, _ := json.Marshal(s.Params)
	if s.Params == nil {
		params = []byte("{}")
	}
	return s.Op + " " + string(params)
}

// Usage is the token count of one or more model turns.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
}

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CacheReadTokens += o.CacheReadTokens
}

// Episode is one (model, arm, task) run: the surface the agent sees, the MCP
// session its calls go through, and the trace they leave.
type Episode struct {
	Task     Task
	Surface  *Surface
	System   string
	MaxTurns int

	session *mcp.ClientSession
	backend Backend
	visible map[string]bool

	Turns     int
	Steps     []Step
	Answer    string
	Usage     Usage
	Exhausted bool

	// attributed counts backend exchanges made during tool calls; any others
	// (at startup, listing tools, after the last call) are graded too.
	attributed int

	// A host that reports its own spend (the claude CLI) sets these; the
	// record then carries that figure instead of one priced from Usage.
	cliCost    float64
	hasCLICost bool
}

// NewEpisode binds an episode to a live session and backend.
func NewEpisode(task Task, surf *Surface, system string, maxTurns int, session *mcp.ClientSession, backend Backend) *Episode {
	vis := map[string]bool{}
	for _, t := range surf.Tools {
		vis[t.Name] = true
	}
	return &Episode{Task: task, Surface: surf, System: system, MaxTurns: maxTurns, session: session, backend: backend, visible: vis}
}

// resultLimit bounds the result text kept per step in the record; the model
// sees the whole result.
const resultLimit = 600

// Call runs one tool call through the MCP session and records it. It returns
// the full result text and the error flag, exactly as a client would hand
// them back to the model. A call to a tool the arm hid, or the server never
// listed, is answered in-band as an error — the model made it, so it counts.
func (e *Episode) Call(ctx context.Context, name string, args map[string]any) (string, bool) {
	step := Step{Turn: e.Turns, Tool: name}
	step.Op, step.Params = normalize(name, args)

	if !e.visible[name] {
		step.IsError, step.Unknown = true, true
		text := fmt.Sprintf("unknown tool %q", name)
		step.Result = text
		e.Steps = append(e.Steps, step)
		return text, true
	}

	mark := e.backend.Len()
	res, err := e.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	step.Requests = e.backend.Since(mark)
	e.attributed += len(step.Requests)
	var text string
	switch {
	case err != nil:
		text = "tool call failed: " + err.Error()
		step.IsError = true
	default:
		text = resultText(res)
		step.IsError = res.IsError
	}
	step.Result = truncate(text, resultLimit)
	e.Steps = append(e.Steps, step)
	return text, step.IsError
}

// Finish records the agent's final reply.
func (e *Episode) Finish(answer string) { e.Answer = answer }

// normalize maps a call to its surface-independent op and params. A gateway
// call carries {"action": "...", "params": {...}} — and a meta tool may take
// its arguments beside the action ({"action": "search", "query": "..."}), so
// top-level keys other than action and params fold into params. Anything
// else is a flat tool whose name is the op and whose arguments are the
// params.
func normalize(tool string, args map[string]any) (string, map[string]any) {
	action, ok := args["action"].(string)
	if !ok || action == "" {
		return tool, args
	}
	params := map[string]any{}
	if p, ok := args["params"].(map[string]any); ok {
		for k, v := range p {
			params[k] = v
		}
	}
	for k, v := range args {
		if k != "action" && k != "params" {
			params[k] = v
		}
	}
	if len(params) == 0 {
		params = nil
	}
	return action, params
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		data, _ := json.Marshal(res.StructuredContent)
		parts = append(parts, string(data))
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
