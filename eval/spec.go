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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/gateway"
)

// ParamSpec is one parameter an action accepts, flattened from the catalog's
// path/query params and request-body properties into a single grading- and
// generation-friendly shape.
type ParamSpec struct {
	Name     string `json:"name"`
	In       string `json:"in"` // "path", "query", or "body"
	Required bool   `json:"required"`
	// RequiredWithBody marks a body property the schema lists as required
	// when the body itself is optional: it must be present once the request
	// carries any body field, and may be omitted with the whole body. The
	// catalog exposes the two separately (body_required vs the schema's own
	// required array), and flattening must not collapse them.
	RequiredWithBody bool   `json:"required_with_body,omitempty"`
	Type             string `json:"type"` // JSON Schema primitive, "" when unconstrained
	Enum             []any  `json:"enum,omitempty"`
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
	// BodyDynamic is true when the request body explicitly permits
	// additional properties (a dictionary body). validateParams then accepts
	// body fields the schema does not name, instead of rejecting them as
	// unknown.
	BodyDynamic bool `json:"body_dynamic,omitempty"`
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
		for _, a := range actions {
			spec, err := describeAction(ctx, session, tool, a.Action)
			if err != nil {
				return nil, err
			}
			if err := reconcileDomainDetail(tool, a, spec); err != nil {
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

// domainAction is the shared metadata the domain-level describe advertises for
// one action: its name plus the safety/summary fields that must agree with the
// per-action detail.
type domainAction struct {
	Action      string `json:"action"`
	Summary     string `json:"summary"`
	ReadOnly    bool   `json:"readonly"`
	Destructive bool   `json:"destructive"`
}

// describeDomain reads the domain-level describe payload: the action list plus
// the summary and safety fields the listing advertises for each.
func describeDomain(ctx context.Context, session *mcp.ClientSession, tool string) ([]domainAction, error) {
	raw, err := describeCall(ctx, session, tool, "")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Actions []domainAction `json:"actions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%s: decode domain describe: %w", tool, err)
	}
	return payload.Actions, nil
}

// reconcileDomainDetail refuses a server whose domain listing and per-action
// detail disagree on the fields both surfaces carry. The eval derives its
// prompt, grading, and oracle from the detail, so a silent divergence would
// let a client read one summary or safety class in the listing while the eval
// scored against another; catch it instead of dropping the listing's copy.
func reconcileDomainDetail(tool string, listed domainAction, detail ActionSpec) error {
	switch {
	case listed.ReadOnly != detail.ReadOnly:
		return fmt.Errorf("%s/%s: domain listing says readonly=%v, action detail says %v", tool, detail.Action, listed.ReadOnly, detail.ReadOnly)
	case listed.Destructive != detail.Destructive:
		return fmt.Errorf("%s/%s: domain listing says destructive=%v, action detail says %v", tool, detail.Action, listed.Destructive, detail.Destructive)
	case strings.TrimSpace(listed.Summary) != strings.TrimSpace(detail.Summary):
		return fmt.Errorf("%s/%s: domain listing summary %q differs from action detail %q", tool, detail.Action, listed.Summary, detail.Summary)
	}
	return nil
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
	// BodyRequired is OpenAPI requestBody.required: whether the body itself
	// must be supplied. Without it the schema's inner required array is
	// conditional on a body being present at all.
	BodyRequired bool `json:"body_required"`
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

	// The per-action describe must name the action we asked about. A server
	// whose domain listing and per-action describe disagree would otherwise
	// have this silently substitute the returned action for the requested
	// one, dropping the listed action from the corpus while a generated
	// oracle run still grades perfectly against the substituted spec.
	if strings.TrimSpace(od.Action) == "" || od.Action != action {
		return ActionSpec{}, fmt.Errorf("%s/%s: describe named action %q, not the requested %q", tool, action, od.Action, action)
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
	spec.Params = append(spec.Params, bodyParams(od.Body, od.BodyRequired)...)
	spec.BodyDynamic = bodyAllowsAdditional(od.Body)
	return spec, nil
}

// bodyAllowsAdditional reports whether a request-body schema explicitly
// permits properties it does not name (additionalProperties: true) — the
// dictionary-body shape StampStrict deliberately preserves. Such an action
// takes arbitrary body fields, so the grader must not reject them as unknown.
func bodyAllowsAdditional(body map[string]any) bool {
	if body == nil {
		return false
	}
	allowed, ok := body["additionalProperties"].(bool)
	return ok && allowed
}

// bodyParams flattens a request-body JSON Schema into body ParamSpecs. The
// schema's required array binds unconditionally only when the body itself is
// required; for an optional body those properties become RequiredWithBody.
func bodyParams(body map[string]any, bodyRequired bool) []ParamSpec {
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
		out = append(out, ParamSpec{
			Name: name, In: "body", Type: typ, Enum: enum,
			Required:         required[name] && bodyRequired,
			RequiredWithBody: required[name] && !bodyRequired,
		})
	}
	return out
}

// schemaTypeEnum extracts the primitive type and the enum constraint from a
// property schema. Enum members are kept in their JSON types (string, number,
// boolean), not narrowed to strings, so an integer or boolean enum still
// constrains the grader.
func schemaTypeEnum(schema map[string]any) (string, []any) {
	if schema == nil {
		return "", nil
	}
	typ, _ := schema["type"].(string)
	enum, _ := schema["enum"].([]any)
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
