// Package eval is a structural evaluation loop for the MCP gateway servers
// built on this toolkit (basecamp/hey/fizzy). It reads a live server's own
// wire surface — the tool listing and the gateway describe payloads — as the
// specification, generates deterministic natural-language scenarios from it,
// asks a cheap model to pick the right {tool, action, params}, and grades the
// answer by rule: correct tool+action, params valid against the catalog
// schema, and safety annotations respected. No backend, judge, or cassette is
// involved: the describe/list surface the eval reads is served from the
// catalog, never the product API, so the whole loop runs hermetically and the
// only spend is the per-scenario model turn.
//
// The eval speaks each catalog's own vocabulary over the wire, so it is
// product-agnostic: point it at any gateway server (in-process fake or a real
// stdio subprocess) and it derives the spec from what that server lists.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/gateway"
)

// ParamSpec is one parameter an action accepts, flattened from the catalog's
// path/query params and request-body properties into a single grading- and
// generation-friendly shape.
type ParamSpec struct {
	Name     string   `json:"name"`
	In       string   `json:"in"` // "path", "query", or "body"
	Required bool     `json:"required"`
	Type     string   `json:"type"` // JSON Schema primitive, "" when unconstrained
	Enum     []string `json:"enum,omitempty"`
}

// ActionSpec is one gateway action fully described: identity, safety
// annotations, and the parameters it accepts. It is the unit the generator
// samples and the grader checks against — derived entirely from the server's
// own describe payloads.
type ActionSpec struct {
	Tool        string      `json:"tool"`
	Action      string      `json:"action"`
	Summary     string      `json:"summary"`
	ReadOnly    bool        `json:"readonly"`
	Destructive bool        `json:"destructive"`
	Idempotent  bool        `json:"idempotent"`
	Paginated   bool        `json:"paginated"`
	Params      []ParamSpec `json:"params"`
}

// RequiredParams returns the names of the action's required parameters, in
// declaration order.
func (a ActionSpec) RequiredParams() []string {
	var names []string
	for _, p := range a.Params {
		if p.Required {
			names = append(names, p.Name)
		}
	}
	return names
}

// param returns the named parameter spec.
func (a ActionSpec) param(name string) (ParamSpec, bool) {
	for _, p := range a.Params {
		if p.Name == name {
			return p, true
		}
	}
	return ParamSpec{}, false
}

// SpecFromSession derives the full action catalog from a live gateway server
// by reading its tool listing and per-action describe payloads. It makes no
// write calls and never reaches the product backend: describe is served from
// the catalog by the gateway itself.
func SpecFromSession(ctx context.Context, session *mcp.ClientSession) ([]ActionSpec, error) {
	var tools []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		tools = append(tools, tool.Name)
	}
	sort.Strings(tools)

	var specs []ActionSpec
	for _, tool := range tools {
		actions, err := describeDomain(ctx, session, tool)
		if err != nil {
			return nil, err
		}
		for _, action := range actions {
			spec, err := describeAction(ctx, session, tool, action)
			if err != nil {
				return nil, err
			}
			specs = append(specs, spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Tool != specs[j].Tool {
			return specs[i].Tool < specs[j].Tool
		}
		return specs[i].Action < specs[j].Action
	})
	return specs, nil
}

// describeDomain lists an action's names for one tool via the domain-level
// describe payload.
func describeDomain(ctx context.Context, session *mcp.ClientSession, tool string) ([]string, error) {
	raw, err := describeCall(ctx, session, tool, "")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Actions []struct {
			Action string `json:"action"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%s: decode domain describe: %w", tool, err)
	}
	var names []string
	for _, a := range payload.Actions {
		names = append(names, a.Action)
	}
	return names, nil
}

// operationDescribe mirrors the subset of the catalog Operation describe
// payload the eval reads. It is intentionally shared by every product server
// because they all render the same gateway describe shape.
type operationDescribe struct {
	Action      string `json:"action"`
	Summary     string `json:"summary"`
	ReadOnly    bool   `json:"readonly"`
	Idempotent  bool   `json:"idempotent"`
	Destructive bool   `json:"destructive"`
	Paginated   bool   `json:"paginated"`
	Params      []struct {
		Name     string         `json:"name"`
		In       string         `json:"in"`
		Required bool           `json:"required"`
		Schema   map[string]any `json:"schema"`
	} `json:"params"`
	Body map[string]any `json:"body"`
}

// describeAction reads one action's full describe payload and flattens it into
// an ActionSpec.
func describeAction(ctx context.Context, session *mcp.ClientSession, tool, action string) (ActionSpec, error) {
	raw, err := describeCall(ctx, session, tool, action)
	if err != nil {
		return ActionSpec{}, err
	}
	var od operationDescribe
	if err := json.Unmarshal(raw, &od); err != nil {
		return ActionSpec{}, fmt.Errorf("%s/%s: decode action describe: %w", tool, action, err)
	}

	spec := ActionSpec{
		Tool:        tool,
		Action:      od.Action,
		Summary:     od.Summary,
		ReadOnly:    od.ReadOnly,
		Destructive: od.Destructive,
		Idempotent:  od.Idempotent,
		Paginated:   od.Paginated,
	}
	for _, p := range od.Params {
		typ, enum := schemaTypeEnum(p.Schema)
		spec.Params = append(spec.Params, ParamSpec{
			Name: p.Name, In: p.In, Required: p.Required, Type: typ, Enum: enum,
		})
	}
	spec.Params = append(spec.Params, bodyParams(od.Body)...)
	return spec, nil
}

// bodyParams flattens a request-body JSON Schema into body ParamSpecs.
func bodyParams(body map[string]any) []ParamSpec {
	if body == nil {
		return nil
	}
	props, _ := body["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	required := map[string]bool{}
	if req, ok := body["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ParamSpec, 0, len(names))
	for _, name := range names {
		schema, _ := props[name].(map[string]any)
		typ, enum := schemaTypeEnum(schema)
		out = append(out, ParamSpec{Name: name, In: "body", Required: required[name], Type: typ, Enum: enum})
	}
	return out
}

// schemaTypeEnum extracts the primitive type and string enum from a property
// schema.
func schemaTypeEnum(schema map[string]any) (string, []string) {
	if schema == nil {
		return "", nil
	}
	typ, _ := schema["type"].(string)
	var enum []string
	if raw, ok := schema["enum"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				enum = append(enum, s)
			}
		}
	}
	return typ, enum
}

// describeCall invokes the gateway describe action and returns the JSON text
// payload. An empty action asks for the domain-level listing.
func describeCall(ctx context.Context, session *mcp.ClientSession, tool, action string) ([]byte, error) {
	params := map[string]any{}
	if action != "" {
		params["action"] = action
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: map[string]any{"action": gateway.DescribeAction, "params": params},
	})
	if err != nil {
		return nil, fmt.Errorf("%s describe %q: %w", tool, action, err)
	}
	if len(res.Content) == 0 {
		return nil, fmt.Errorf("%s describe %q: empty result", tool, action)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return nil, fmt.Errorf("%s describe %q: non-text result %T", tool, action, res.Content[0])
	}
	if res.IsError {
		return nil, fmt.Errorf("%s describe %q: %s", tool, action, text.Text)
	}
	return []byte(text.Text), nil
}
