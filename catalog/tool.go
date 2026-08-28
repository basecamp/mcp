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

// ToolTitle returns the human-readable tool title, e.g. "HEY Todos".
func (d *Domain) ToolTitle() string {
	return d.Title
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
	filtered := &Domain{Key: d.Key, Tool: d.Tool, Title: d.Title, Blurb: d.Blurb}
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

// AllIdempotent reports whether repeating any call is safe: every write
// operation is idempotent. Read operations count as idempotent regardless of
// their transport traits, so an all-read domain is vacuously idempotent.
func (d *Domain) AllIdempotent() bool {
	for _, op := range d.Operations {
		if !op.ReadOnly && !op.Idempotent {
			return false
		}
	}
	return true
}

// AnyDestructive reports whether any operation deletes or irreversibly
// alters data.
func (d *Domain) AnyDestructive() bool {
	for _, op := range d.Operations {
		if op.Destructive {
			return true
		}
	}
	return false
}

// SplitReadWrite partitions the domain into a read tool and a write tool —
// the shape both connector directories require: read and write operations
// must not share a tool, or its safety annotations cannot be truthful. The
// read half keeps the domain's key and tool name; the write half appends
// "_write". A half with no operations is nil.
func (d *Domain) SplitReadWrite() (read, write *Domain) {
	read = &Domain{Key: d.Key, Tool: d.Tool, Title: d.Title, Blurb: d.Blurb}
	write = &Domain{Key: d.Key + "_write", Tool: d.Tool + "_write", Title: d.Title + " (writes)", Blurb: d.Blurb}
	for _, op := range d.Operations {
		if op.ReadOnly {
			read.Operations = append(read.Operations, op)
		} else {
			write.Operations = append(write.Operations, op)
		}
	}
	if len(write.Operations) == 0 {
		return read, nil
	}
	if len(read.Operations) == 0 {
		return nil, write
	}
	read.Counterpart = write.Tool
	write.Counterpart = read.Tool
	return read, write
}

// SplitReadWrite splits every domain, preserving order: each domain's read
// tool, then its write tool, halves with no operations omitted.
func SplitReadWrite(domains []*Domain) []*Domain {
	var out []*Domain
	for _, d := range domains {
		read, write := d.SplitReadWrite()
		if read != nil {
			out = append(out, read)
		}
		if write != nil {
			out = append(out, write)
		}
	}
	return out
}

// Description renders the generated tool description: the domain blurb, the
// gateway calling convention, and a one-line summary per action.
func (d *Domain) Description() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", d.Blurb)
	b.WriteString("Gateway tool: call with {\"action\": \"...\", \"params\": {...}}.\n")
	fmt.Fprintf(&b, "Call {\"action\": %q, \"params\": {\"action\": \"NAME\"}} for an action's full parameter schema.\n", gateway.DescribeAction)
	if d.Counterpart != "" {
		// "When served": domain narrowing can exclude the sibling, and this
		// rendering cannot see the final served set — so point at the
		// counterpart without asserting its presence.
		if d.AllReadOnly() {
			fmt.Fprintf(&b, "This tool only reads; the domain's write actions, when served, live in the %s tool.\n", d.Counterpart)
		} else {
			fmt.Fprintf(&b, "This tool writes; the domain's read actions, when served, live in the %s tool.\n", d.Counterpart)
		}
	}
	b.WriteString("\nACTIONS (RO = read-only):\n")
	for _, op := range d.Operations {
		var notes []string
		if op.ReadOnly {
			notes = append(notes, "RO")
		}
		if op.Destructive {
			notes = append(notes, "destructive")
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
				"action":      op.Action,
				"summary":     op.Summary,
				"readonly":    op.ReadOnly,
				"destructive": op.Destructive,
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
