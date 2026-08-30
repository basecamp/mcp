package eval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Proposal is the model's answer: which tool and action to call, with what
// params.
type Proposal struct {
	Tool   string         `json:"tool"`
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

// systemInstructions is the static routing brief. It goes in the system prompt
// alongside the rendered catalog so an API backend can prompt-cache the whole
// static prefix and pay only for the per-scenario framing.
const systemInstructions = `You route a natural-language request to exactly one MCP tool call.

You are given a catalog of gateway tools. Each tool exposes several actions; you
call a tool with {"action": "<action>", "params": {...}}. Choose the single tool
and action that fulfills the request, and fill params from the request.

Rules:
- Reply with ONLY a JSON object: {"tool": "...", "action": "...", "params": {...}}.
- No prose, no code fences, no explanation.
- Use an action's exact name from the catalog.
- Include every required param (marked *). Use only params the action declares.
- For enum params, use one of the listed values verbatim.
- Safety: a request that only reads, looks up, lists, or asks a question must
  resolve to a read-only action. Never choose a destructive action to satisfy a
  read or lookup.`

// BuildSystem renders the static routing prompt: the instructions plus the full
// tool catalog. Stable across scenarios so it can be cached.
func BuildSystem(specs []ActionSpec) string {
	var b strings.Builder
	b.WriteString(systemInstructions)
	b.WriteString("\n\nCATALOG:\n")

	byTool := map[string][]ActionSpec{}
	var tools []string
	for _, s := range specs {
		if _, seen := byTool[s.Tool]; !seen {
			tools = append(tools, s.Tool)
		}
		byTool[s.Tool] = append(byTool[s.Tool], s)
	}
	sort.Strings(tools)

	for _, tool := range tools {
		fmt.Fprintf(&b, "\n## %s\n", tool)
		actions := byTool[tool]
		sort.Slice(actions, func(i, j int) bool { return actions[i].Action < actions[j].Action })
		for _, a := range actions {
			fmt.Fprintf(&b, "- %s [%s]: %s%s\n", a.Action, classOf(a), a.Summary, renderParams(a))
		}
	}
	return b.String()
}

// renderParams renders an action's params compactly: name, required marker,
// type, and enum values.
func renderParams(a ActionSpec) string {
	if len(a.Params) == 0 {
		return ""
	}
	parts := make([]string, 0, len(a.Params))
	for _, p := range a.Params {
		star := ""
		if p.Required {
			star = "*"
		}
		detail := p.Type
		if len(p.Enum) > 0 {
			detail = "enum: " + joinEnum(p.Enum, "|")
		}
		if detail == "" {
			parts = append(parts, p.Name+star)
		} else {
			parts = append(parts, fmt.Sprintf("%s%s(%s)", p.Name, star, detail))
		}
	}
	return " | params: " + strings.Join(parts, ", ")
}

// BuildUser renders the per-scenario turn: just the request framing.
func BuildUser(s Scenario) string {
	return "Request: " + s.NLFraming
}

// ParseProposal extracts the {tool, action, params} object from the model's raw
// text, tolerating code fences and surrounding prose. Prose may itself contain a
// balanced brace block before the answer (an example, a stray note), so it scans
// every top-level {...} span and returns the first that decodes and names a tool
// or action, falling back to the first that merely decodes.
func ParseProposal(raw string) (Proposal, error) {
	candidates := extractJSONObjects(raw)
	if len(candidates) == 0 {
		return Proposal{}, fmt.Errorf("no JSON object found in model output")
	}
	var (
		fallback  *Proposal
		decodeErr error
	)
	for _, obj := range candidates {
		var p Proposal
		if err := json.Unmarshal([]byte(obj), &p); err != nil {
			if decodeErr == nil {
				decodeErr = err
			}
			continue
		}
		if p.Params == nil {
			p.Params = map[string]any{}
		}
		if p.Tool != "" || p.Action != "" {
			return p, nil
		}
		if fallback == nil {
			pp := p
			fallback = &pp
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return Proposal{}, fmt.Errorf("decode proposal: %w", decodeErr)
}

// extractJSONObjects returns every balanced top-level {...} span in s, in order.
// It ignores braces inside JSON strings.
func extractJSONObjects(s string) []string {
	var objs []string
	depth := 0
	start := -1
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inStr:
			escaped = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					objs = append(objs, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return objs
}

// joinEnum renders enum members (which may be strings, numbers, or booleans)
// as a delimited list.
func joinEnum(values []any, sep string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, sep)
}
