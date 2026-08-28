package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/basecamp/mcp/gateway"
)

// The catalog is a gateway catalog: each derived Domain is served directly
// by gateway.New via Catalog.GatewayDomains.
var _ gateway.Domain = (*Domain)(nil)

// Name returns the short domain key, e.g. "todos".
func (d *Domain) Name() string {
	return d.Key
}

// ToolName returns the MCP tool name, e.g. "hey_todos".
func (d *Domain) ToolName() string {
	return d.Tool
}

// Find returns the dispatch surface of the operation registered under the
// given action name.
func (d *Domain) Find(action string) (gateway.Operation, bool) {
	op, ok := d.Operation(action)
	if !ok {
		return gateway.Operation{}, false
	}
	return gateway.Operation{Action: op.Action, ReadOnly: op.ReadOnly}, true
}

// FilterReadOnly returns a copy of the domain containing only read-only
// operations, reporting false when none remain.
func (d *Domain) FilterReadOnly() (gateway.Domain, bool) {
	filtered := &Domain{Key: d.Key, Tool: d.Tool, Blurb: d.Blurb}
	for _, op := range d.Operations {
		if op.ReadOnly {
			filtered.Operations = append(filtered.Operations, op)
		}
	}
	if len(filtered.Operations) == 0 {
		return nil, false
	}
	return filtered, true
}

// Operation returns the operation registered under the given action name.
func (d *Domain) Operation(action string) (*Operation, bool) {
	for _, op := range d.Operations {
		if op.Action == action {
			return op, true
		}
	}
	return nil, false
}

// AllReadOnly reports whether every operation in the domain is read-only.
func (d *Domain) AllReadOnly() bool {
	for _, op := range d.Operations {
		if !op.ReadOnly {
			return false
		}
	}
	return true
}

// Description renders the generated tool description: the domain blurb, the
// gateway calling convention, and a one-line summary per action.
func (d *Domain) Description() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", d.Blurb)
	b.WriteString("Gateway tool: call with {\"action\": \"...\", \"params\": {...}}.\n")
	fmt.Fprintf(&b, "Call {\"action\": %q, \"params\": {\"action\": \"NAME\"}} for an action's full parameter schema.\n\n", gateway.DescribeAction)
	b.WriteString("ACTIONS (RO = read-only):\n")
	for _, op := range d.Operations {
		var notes []string
		if op.ReadOnly {
			notes = append(notes, "RO")
		}
		if op.Paginated {
			notes = append(notes, "paginated")
		}
		suffix := ""
		if len(notes) > 0 {
			suffix = " (" + strings.Join(notes, ", ") + ")"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", op.Action, suffix, op.Summary)
	}
	return b.String()
}

// InputSchema renders the generated JSON Schema for the gateway tool's
// arguments. Per-action parameter and body schemas are served on demand via
// the describe action rather than inlined here, keeping tools/list small.
func (d *Domain) InputSchema() map[string]any {
	actions := make([]any, 0, len(d.Operations)+1)
	for _, op := range d.Operations {
		actions = append(actions, op.Action)
	}
	actions = append(actions, gateway.DescribeAction)
	return map[string]any{
		"type":                 "object",
		"required":             []any{"action"},
		"additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": actions,
			},
			"params": map[string]any{
				"type":        "object",
				"description": "Parameters for the action. Call describe for the action's schema.",
			},
		},
	}
}

// Describe returns the describe payload for one action, or for the whole
// domain when action is empty.
func (d *Domain) Describe(action string) (any, error) {
	if action == "" {
		summaries := make([]map[string]any, 0, len(d.Operations))
		for _, op := range d.Operations {
			summaries = append(summaries, map[string]any{
				"action":   op.Action,
				"summary":  op.Summary,
				"readonly": op.ReadOnly,
			})
		}
		return map[string]any{"domain": d.Key, "actions": summaries}, nil
	}
	op, ok := d.Operation(action)
	if !ok {
		return nil, fmt.Errorf("unknown action %q in domain %q (actions: %s)", action, d.Key, strings.Join(d.ActionNames(), ", "))
	}
	return op, nil
}

// ActionNames returns the domain's action names in sorted order.
func (d *Domain) ActionNames() []string {
	names := make([]string, 0, len(d.Operations))
	for _, op := range d.Operations {
		names = append(names, op.Action)
	}
	sort.Strings(names)
	return names
}
