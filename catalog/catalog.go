// Package catalog derives a product's MCP tool catalog from its SDK model
// exports.
//
// The catalog is generated, not hand-written: the SDK's behavior-model.json
// supplies per-operation traits (readonly, idempotent, pagination) and its
// openapi.json supplies identity, wire shape, and documentation (operationId,
// method, path, tags, descriptions, parameter and body schemas). Both files
// are build products of the SDK's Smithy model, vendored by the product
// server. Joining them yields domain gateway tools in the gateway package's
// style — without ~15 hand-written domain files per product.
//
// Extracted from hey-mcp-server's internal/catalog, whose derived tool
// surface matched the shape basecamp-mcp-server maintains by hand. The
// product supplies a Spec: its tool prefix, its curated domain specs, and
// its vendored model snapshot as an fs.FS.
//
// The loader's contract is deliberately bounded to the subset of OpenAPI the
// producing SDK exports emit: per-operation inline parameters and bodies,
// schema-located ("#/components/schemas/*") references, exactly one tag per
// operation. Anything outside that subset — Reference Objects for
// parameters or bodies, path-level parameters, unresolvable or
// non-schema-located refs, unconstrained body schemas — fails loudly at
// Load rather than degrading the derived catalog. Widening the subset is a
// deliberate change made with a real export in hand, not a fallback.
package catalog

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"unicode"

	"github.com/basecamp/mcp/gateway"
)

// DomainSpec curates which slice of the SDK surface one domain gateway tool
// exposes. Tags are the SDK's OpenAPI tags (each operation carries exactly
// one); a spec may merge several tags into one tool. This mapping is the only
// hand-maintained part of the catalog — everything else derives from the SDK
// model. Tags left unmapped are reported in Catalog.Unmapped, so growing the
// surface is a one-line change in the product's specs.
type DomainSpec struct {
	Key   string // short domain name, e.g. "boxes"
	Blurb string // first line of the tool description
	Tags  []string
}

// Spec parameterizes the catalog for one product.
type Spec struct {
	// ToolPrefix prefixes each domain key to form the MCP tool name,
	// e.g. "hey_" yields tool "hey_boxes" for domain "boxes".
	ToolPrefix string
	// Domains is the curated tag-to-domain mapping, in tool display order.
	Domains []DomainSpec
	// Model is the product's vendored model snapshot: an fs.FS with
	// behavior-model.json and openapi.json at its root.
	Model fs.FS
}

// behaviorModel mirrors the subset of the SDK's behavior-model.json we read.
type behaviorModel struct {
	Version    string                    `json:"version"`
	Operations map[string]behaviorTraits `json:"operations"`
}

type behaviorTraits struct {
	ReadOnly   bool `json:"readonly"`
	Idempotent bool `json:"idempotent"`
	Pagination *struct {
		Style string `json:"style"`
	} `json:"pagination"`
}

// openapiDoc mirrors the subset of the SDK's openapi.json we read.
type openapiDoc struct {
	Paths      map[string]map[string]*openapiOperation `json:"paths"`
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
}

type openapiOperation struct {
	OperationID string   `json:"operationId"`
	Tags        []string `json:"tags"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Parameters  []Param  `json:"parameters"`
	RequestBody *struct {
		// Ref captures the reusable Reference Object form, which this
		// loader does not resolve: neither producing SDK emits it, and a
		// silently dropped body would be worse than a startup error.
		Ref      string `json:"$ref"`
		Required bool   `json:"required"`
		Content  map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

// Param is one query or path parameter of an operation.
type Param struct {
	// Ref captures the reusable Reference Object form ({"$ref": ...}),
	// which the loader refuses at startup rather than silently dropping.
	// Never present in a built catalog.
	Ref         string         `json:"$ref,omitempty"`
	Name        string         `json:"name"`
	In          string         `json:"in"`
	Required    bool           `json:"required,omitempty"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema"`
}

// Operation is one SDK operation joined across the behavior model and the
// OpenAPI export: everything a gateway tool needs to list, describe, and
// eventually dispatch it.
type Operation struct {
	ID         string  `json:"operation"` // SDK operationId, e.g. "AdvancedSearch"
	Action     string  `json:"action"`    // gateway action name, e.g. "advanced_search"
	Tag        string  `json:"tag"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Summary    string  `json:"summary"`
	Doc        string  `json:"doc,omitempty"`
	ReadOnly   bool    `json:"readonly"`
	Idempotent bool    `json:"idempotent"`
	Paginated  bool    `json:"paginated"`
	Params     []Param `json:"params,omitempty"`
	// Body is the resolved request body schema ($refs inlined), nil when the
	// operation takes no body.
	Body map[string]any `json:"body,omitempty"`
	// BodyRequired reports whether the request body itself must be supplied
	// (OpenAPI requestBody.required). The schema's own "required" array only
	// constrains properties once a body exists; this flag says the body must.
	BodyRequired bool `json:"body_required,omitempty"`
}

// Domain is one gateway tool: a curated group of operations exposed as a
// single MCP tool with resource+action-style dispatch.
type Domain struct {
	Key        string       // short name, e.g. "boxes"
	Tool       string       // MCP tool name, e.g. "hey_boxes"
	Blurb      string       // first line of the tool description
	Operations []*Operation // sorted by action name
}

// Catalog is the full derived tool surface plus the leftovers report: which
// model operations the curated domains deliberately do not expose yet.
type Catalog struct {
	ModelVersion string
	Domains      []*Domain
	// Unmapped groups the operations whose tag no DomainSpec claims, keyed by
	// tag. They are counted, not lost: tests fail when the SDK grows a tag
	// nobody has decided about.
	Unmapped map[string][]string
}

// GatewayDomains returns the catalog's domains ready to hand to gateway.New.
func (c *Catalog) GatewayDomains() []gateway.Domain {
	domains := make([]gateway.Domain, len(c.Domains))
	for i, d := range c.Domains {
		domains[i] = d
	}
	return domains
}

// Load derives the catalog from the spec's model snapshot.
func Load(spec Spec) (*Catalog, error) {
	if spec.Model == nil {
		return nil, fmt.Errorf("catalog: Spec.Model is required")
	}

	var bm behaviorModel
	if err := unmarshalModel(spec.Model, "behavior-model.json", &bm); err != nil {
		return nil, err
	}
	var oa openapiDoc
	if err := unmarshalModel(spec.Model, "openapi.json", &oa); err != nil {
		return nil, err
	}

	ops, err := joinOperations(&bm, &oa)
	if err != nil {
		return nil, err
	}

	cat := &Catalog{ModelVersion: bm.Version, Unmapped: map[string][]string{}}
	claimed := map[string]string{} // tag -> domain key
	seenKeys := map[string]bool{}
	for _, ds := range spec.Domains {
		if seenKeys[ds.Key] {
			return nil, fmt.Errorf("duplicate domain key %q", ds.Key)
		}
		seenKeys[ds.Key] = true
		domain := &Domain{Key: ds.Key, Tool: spec.ToolPrefix + ds.Key, Blurb: ds.Blurb}
		for _, tag := range ds.Tags {
			if prev, ok := claimed[tag]; ok {
				return nil, fmt.Errorf("tag %q claimed by both %q and %q", tag, prev, ds.Key)
			}
			claimed[tag] = ds.Key
			tagOps, ok := ops[tag]
			if !ok {
				return nil, fmt.Errorf("domain %q claims tag %q, which is not in the model", ds.Key, tag)
			}
			domain.Operations = append(domain.Operations, tagOps...)
		}
		sort.Slice(domain.Operations, func(i, j int) bool {
			return domain.Operations[i].Action < domain.Operations[j].Action
		})
		if err := checkActionNames(domain); err != nil {
			return nil, err
		}
		cat.Domains = append(cat.Domains, domain)
	}

	for tag, tagOps := range ops {
		if _, ok := claimed[tag]; ok {
			continue
		}
		var ids []string
		for _, op := range tagOps {
			ids = append(ids, op.ID)
		}
		sort.Strings(ids)
		cat.Unmapped[tag] = ids
	}

	return cat, nil
}

func unmarshalModel(fsys fs.FS, name string, v any) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("model %s: %w", name, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

// joinOperations joins the two model files on operationId and groups the
// result by primary tag. The join is strict both ways: an operation present
// in one file but not the other means the vendored snapshot is torn, and the
// catalog refuses to build from it.
func joinOperations(bm *behaviorModel, oa *openapiDoc) (map[string][]*Operation, error) {
	byTag := map[string][]*Operation{}
	seen := map[string]bool{}
	for path, methods := range oa.Paths {
		for method, op := range methods {
			if op == nil {
				return nil, fmt.Errorf("%s %s: null operation", method, path)
			}
			if op.OperationID == "" {
				return nil, fmt.Errorf("%s %s: missing operationId", method, path)
			}
			if op.RequestBody != nil && op.RequestBody.Ref != "" {
				return nil, fmt.Errorf("operation %q: requestBody $ref is not supported (inline the body in the SDK export)", op.OperationID)
			}
			if seen[op.OperationID] {
				return nil, fmt.Errorf("duplicate operationId %q", op.OperationID)
			}
			seen[op.OperationID] = true

			traits, ok := bm.Operations[op.OperationID]
			if !ok {
				return nil, fmt.Errorf("operation %q in openapi.json but not behavior-model.json", op.OperationID)
			}
			if len(op.Tags) != 1 {
				return nil, fmt.Errorf("operation %q: expected exactly one tag, got %v", op.OperationID, op.Tags)
			}

			params, err := resolveParams(op.Parameters, oa)
			if err != nil {
				return nil, fmt.Errorf("operation %q: %w", op.OperationID, err)
			}
			body, err := resolveBody(op, oa)
			if err != nil {
				return nil, fmt.Errorf("operation %q: %w", op.OperationID, err)
			}
			joined := &Operation{
				ID:           op.OperationID,
				Action:       snakeCase(op.OperationID),
				Tag:          op.Tags[0],
				Method:       strings.ToUpper(method),
				Path:         path,
				Summary:      summarize(op.Summary, op.Description),
				Doc:          op.Description,
				ReadOnly:     traits.ReadOnly,
				Idempotent:   traits.Idempotent,
				Paginated:    traits.Pagination != nil,
				Params:       params,
				Body:         body,
				BodyRequired: op.RequestBody != nil && op.RequestBody.Required,
			}
			byTag[joined.Tag] = append(byTag[joined.Tag], joined)
		}
	}

	for id := range bm.Operations {
		if !seen[id] {
			return nil, fmt.Errorf("operation %q in behavior-model.json but not openapi.json", id)
		}
	}

	return byTag, nil
}

func checkActionNames(d *Domain) error {
	seen := map[string]string{gateway.DescribeAction: "(reserved)"}
	for _, op := range d.Operations {
		if prev, ok := seen[op.Action]; ok {
			return fmt.Errorf("domain %q: action %q from %s collides with %s", d.Key, op.Action, op.ID, prev)
		}
		seen[op.Action] = op.ID
	}
	return nil
}

// resolveParams returns the operation's parameters with schema $refs inlined.
func resolveParams(params []Param, oa *openapiDoc) ([]Param, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make([]Param, len(params))
	for i, p := range params {
		if p.Ref != "" {
			return nil, fmt.Errorf("parameter $ref %q is not supported (inline parameters in the SDK export)", p.Ref)
		}
		resolved, err := resolveRefs(p.Schema, oa, 0)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", p.Name, err)
		}
		p.Schema = asSchema(resolved)
		out[i] = p
	}
	return out, nil
}

// resolveBody returns the operation's JSON request body schema with $refs
// inlined, or nil when the operation takes no JSON body.
func resolveBody(op *openapiOperation, oa *openapiDoc) (map[string]any, error) {
	if op.RequestBody == nil {
		return nil, nil
	}
	content, ok := op.RequestBody.Content["application/json"]
	if !ok {
		return nil, nil
	}
	resolved, err := resolveRefs(content.Schema, oa, 0)
	if err != nil {
		return nil, fmt.Errorf("request body: %w", err)
	}
	schema := asSchema(resolved)
	if len(schema) == 0 {
		// An unconstrained ({}) or missing body schema would vanish under
		// body's omitempty, leaving clients unable to distinguish
		// "arbitrary JSON body" from "no body". The SDK exports always
		// constrain bodies; refuse the shape they never emit.
		return nil, fmt.Errorf("request body has no schema (the SDK exports always constrain JSON bodies)")
	}
	return schema, nil
}

// maxRefDepth caps $ref inlining. Deeper (usually recursive) structures end
// in a self-contained truncation stub rather than expanding forever.
const maxRefDepth = 8

// resolveRefs inlines "#/components/schemas/*" references so describe results
// are self-contained, and drops "x-go-*" codegen extensions, which are noise
// to MCP clients. Sibling keys alongside a $ref (the model carries codegen
// hints there) are overlaid on the resolved target. Anything unresolvable —
// unknown ref targets, exhausted depth — passes through untouched.
func resolveRefs(v any, oa *openapiDoc, depth int) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			name := strings.TrimPrefix(ref, "#/components/schemas/")
			if name == ref {
				return nil, fmt.Errorf("unsupported $ref %q (only #/components/schemas/* is emitted by the SDK exports)", ref)
			}
			target, found := oa.Components.Schemas[name]
			if !found {
				return nil, fmt.Errorf("unresolvable $ref %q", ref)
			}
			if depth >= maxRefDepth {
				// Deeper (usually recursive) structures terminate in a
				// self-contained stub: describe never returns the
				// components object, so a retained $ref would dangle.
				return map[string]any{
					"type":        "object",
					"description": fmt.Sprintf("truncated: recursive reference to %s", name),
				}, nil
			}
			rv, err := resolveRefs(deepCopy(target), oa, depth+1)
			if err != nil {
				return nil, err
			}
			resolved, _ := rv.(map[string]any)
			for k, val := range t {
				if k == "$ref" || strings.HasPrefix(k, "x-go-") {
					continue
				}
				sv, err := resolveRefs(val, oa, depth)
				if err != nil {
					return nil, err
				}
				resolved[k] = sv
			}
			return resolved, nil
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			if strings.HasPrefix(k, "x-go-") {
				continue
			}
			sv, err := resolveRefs(val, oa, depth)
			if err != nil {
				return nil, err
			}
			out[k] = sv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			sv, err := resolveRefs(val, oa, depth)
			if err != nil {
				return nil, err
			}
			out[i] = sv
		}
		return out, nil
	default:
		return v, nil
	}
}

func deepCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = deepCopy(t)
		case []any:
			cp := make([]any, len(t))
			for i, e := range t {
				if em, ok := e.(map[string]any); ok {
					cp[i] = deepCopy(em)
				} else {
					cp[i] = e
				}
			}
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}

func asSchema(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// snakeCase converts an SDK operationId to a gateway action name:
// "GetImboxSeen" -> "get_imbox_seen".
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// summarize picks the operation's one-line summary: the explicit summary if
// present, else the first sentence of the description's first paragraph. SDK
// descriptions are hard-wrapped, so a bare first line would truncate
// mid-sentence; the paragraph is unwrapped before cutting at a sentence end.
func summarize(summary, description string) string {
	if summary != "" {
		return summary
	}
	paragraph, _, _ := strings.Cut(description, "\n\n")
	unwrapped := strings.Join(strings.Fields(paragraph), " ")
	if sentence, _, found := strings.Cut(unwrapped, ". "); found {
		unwrapped = sentence
	}
	return strings.TrimSuffix(unwrapped, ".")
}
