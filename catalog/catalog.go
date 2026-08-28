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
	Title string // human-readable tool title, e.g. "HEY Boxes"
	Blurb string // first line of the tool description
	Tags  []string
	// DestructiveActions marks actions destructive by name: the curated
	// override for operations whose behavior model does not yet carry the
	// destructive trait and whose name the bridge classifier misses (hey's
	// empty_trash, say). Stale curation fails loudly: naming an unknown
	// action, a read-only action, or an action whose model already declares
	// the trait is a load error, so overrides die as the model learns.
	DestructiveActions []string
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
	// Destructive is tri-state: the SDK behavior models are only beginning
	// to carry the trait, so absent (nil) falls back to BridgeDestructive.
	Destructive *bool           `json:"destructive"`
	Sensitive   bool            `json:"sensitive"`
	Pagination  *Pagination     `json:"pagination"`
	Retry       *Retry          `json:"retry"`
	EmptyOn     []int           `json:"empty_on"`
	Write       *WriteSemantics `json:"write"`
}

// Pagination is how a list operation pages, in the behavior model's own
// vocabulary (mixed-case keys are the model's, kept verbatim so describe
// payloads match what the SDK documents).
type Pagination struct {
	// Style is the pagination mechanism: "link" (Link header), "cursor",
	// or "page".
	Style string `json:"style"`
	// PageParam names the query parameter carrying the page number, for
	// style "page".
	PageParam string `json:"pageParam,omitempty"`
	// TotalCountHeader names the response header carrying the total count.
	TotalCountHeader string `json:"totalCountHeader,omitempty"`
	// MaxPageSize is the server's page-size cap.
	MaxPageSize int `json:"maxPageSize,omitempty"`
	// Key is the response-object key holding the paginated array, when the
	// response is a wrapper object rather than a bare array.
	Key string `json:"key,omitempty"`
}

// Retry is the operation's declared retry policy.
type Retry struct {
	Max         int    `json:"max,omitempty"`
	BaseDelayMs int    `json:"base_delay_ms,omitempty"`
	Backoff     string `json:"backoff,omitempty"`
	RetryOn     []int  `json:"retry_on,omitempty"`
}

// WriteSemantics is how the server interprets a write's request body. Mode
// "replace" with ClearsOmitted true is the sharpest correctness hint an
// agent can get: any writable field omitted from the request is cleared
// server-side, except the fields listed in PreservedOnOmission.
type WriteSemantics struct {
	Mode                string   `json:"mode"`
	ClearsOmitted       bool     `json:"clearsOmitted,omitempty"`
	PreservedOnOmission []string `json:"preservedOnOmission,omitempty"`
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
	ID         string `json:"operation"` // SDK operationId, e.g. "AdvancedSearch"
	Action     string `json:"action"`    // gateway action name, e.g. "advanced_search"
	Tag        string `json:"tag"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Summary    string `json:"summary"`
	Doc        string `json:"doc,omitempty"`
	ReadOnly   bool   `json:"readonly"`
	Idempotent bool   `json:"idempotent"`
	// Destructive reports whether the operation deletes or irreversibly
	// alters data. Sourced from the behavior model's destructive trait when
	// present; bridged from the action name otherwise (BridgeDestructive).
	Destructive bool `json:"destructive"`
	// Sensitive marks operations touching data the model flags as sensitive.
	Sensitive bool `json:"sensitive,omitempty"`
	Paginated bool `json:"paginated"`
	// Pagination carries the mechanics behind Paginated: style, page-size
	// cap, and where the items live in the response.
	Pagination *Pagination `json:"pagination,omitempty"`
	Retry      *Retry      `json:"retry,omitempty"`
	// EmptyOn lists HTTP statuses that mean "empty result", not "error".
	EmptyOn []int `json:"empty_on,omitempty"`
	// WriteSemantics declares how the server treats the request body on
	// write — most importantly whether omitted fields are cleared.
	WriteSemantics *WriteSemantics `json:"write_semantics,omitempty"`
	Params         []Param         `json:"params,omitempty"`
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
	Key   string // short name, e.g. "boxes"
	Tool  string // MCP tool name, e.g. "hey_boxes"
	Title string // human-readable tool title, e.g. "HEY Boxes"
	Blurb string // first line of the tool description
	// Counterpart names the sibling tool created by SplitReadWrite, so each
	// half's description can point at the other. Empty when unsplit.
	Counterpart string
	Operations  []*Operation // sorted by action name
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

	ops, declared, err := joinOperations(&bm, &oa)
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
		if len(ds.Tags) == 0 {
			return nil, fmt.Errorf("domain %q claims no tags", ds.Key)
		}
		if strings.TrimSpace(ds.Title) == "" {
			return nil, fmt.Errorf("domain %q has no title (connector directories require a title on every tool)", ds.Key)
		}
		domain := &Domain{Key: ds.Key, Tool: spec.ToolPrefix + ds.Key, Title: ds.Title, Blurb: ds.Blurb}
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
		for _, action := range ds.DestructiveActions {
			op, ok := domain.Operation(action)
			if !ok {
				return nil, fmt.Errorf("domain %q marks unknown action %q destructive", ds.Key, action)
			}
			if op.ReadOnly {
				return nil, fmt.Errorf("domain %q marks read-only action %q destructive", ds.Key, action)
			}
			if declared[op.ID] {
				return nil, fmt.Errorf("domain %q overrides action %q, but the model already declares its destructive trait — delete the override", ds.Key, action)
			}
			op.Destructive = true
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
func joinOperations(bm *behaviorModel, oa *openapiDoc) (map[string][]*Operation, map[string]bool, error) {
	byTag := map[string][]*Operation{}
	declared := map[string]bool{}
	seen := map[string]bool{}
	for path, methods := range oa.Paths {
		for method, op := range methods {
			if op == nil {
				return nil, nil, fmt.Errorf("%s %s: null operation", method, path)
			}
			if op.OperationID == "" {
				return nil, nil, fmt.Errorf("%s %s: missing operationId", method, path)
			}
			if op.RequestBody != nil && op.RequestBody.Ref != "" {
				return nil, nil, fmt.Errorf("operation %q: requestBody $ref is not supported (inline the body in the SDK export)", op.OperationID)
			}
			if seen[op.OperationID] {
				return nil, nil, fmt.Errorf("duplicate operationId %q", op.OperationID)
			}
			seen[op.OperationID] = true

			traits, ok := bm.Operations[op.OperationID]
			if !ok {
				return nil, nil, fmt.Errorf("operation %q in openapi.json but not behavior-model.json", op.OperationID)
			}
			if traits.Destructive != nil {
				declared[op.OperationID] = true
			}
			if len(op.Tags) != 1 {
				return nil, nil, fmt.Errorf("operation %q: expected exactly one tag, got %v", op.OperationID, op.Tags)
			}

			params, err := resolveParams(op.Parameters, oa)
			if err != nil {
				return nil, nil, fmt.Errorf("operation %q: %w", op.OperationID, err)
			}
			body, err := resolveBody(op, oa)
			if err != nil {
				return nil, nil, fmt.Errorf("operation %q: %w", op.OperationID, err)
			}
			action := snakeCase(op.OperationID)
			destructive := traits.Destructive != nil && *traits.Destructive
			if traits.Destructive == nil {
				// Bridge until the SDK models carry the trait; the model
				// wins the moment it appears.
				destructive = !traits.ReadOnly && BridgeDestructive(action)
			}
			if traits.ReadOnly && destructive {
				// Contradictory annotations resolve differently across MCP
				// clients; refuse the shape rather than emit it.
				return nil, nil, fmt.Errorf("operation %q is declared both readonly and destructive", op.OperationID)
			}
			joined := &Operation{
				ID:             op.OperationID,
				Action:         action,
				Tag:            op.Tags[0],
				Method:         strings.ToUpper(method),
				Path:           path,
				Summary:        summarize(op.Summary, op.Description),
				Doc:            op.Description,
				ReadOnly:       traits.ReadOnly,
				Idempotent:     traits.Idempotent,
				Destructive:    destructive,
				Sensitive:      traits.Sensitive,
				Paginated:      traits.Pagination != nil,
				Pagination:     traits.Pagination,
				Retry:          traits.Retry,
				EmptyOn:        traits.EmptyOn,
				WriteSemantics: traits.Write,
				Params:         params,
				Body:           body,
				BodyRequired:   op.RequestBody != nil && op.RequestBody.Required,
			}
			byTag[joined.Tag] = append(byTag[joined.Tag], joined)
		}
	}

	for id := range bm.Operations {
		if !seen[id] {
			return nil, nil, fmt.Errorf("operation %q in behavior-model.json but not openapi.json", id)
		}
	}

	return byTag, declared, nil
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
		if p.Name == "" || p.In == "" {
			return nil, fmt.Errorf("parameter (name %q, in %q) is missing its identity", p.Name, p.In)
		}
		if p.Schema == nil {
			return nil, fmt.Errorf("parameter %q has no inline schema (content-based parameters are not in the SDK exports)", p.Name)
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
		types := make([]string, 0, len(op.RequestBody.Content))
		for mt := range op.RequestBody.Content {
			types = append(types, mt)
		}
		sort.Strings(types)
		return nil, fmt.Errorf("request body media types %v are not supported (the SDK exports emit application/json)", types)
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
	StampStrict(schema)
	return schema, nil
}

// StampStrict closes a body object schema: when the top level declares no
// additionalProperties, it becomes false, so clients validate unknown fields
// instead of silently passing them. Only the top level is stamped — nested
// schemas keep whatever the SDK emitted; closing those belongs to the
// emitter (Smithy closed structures), not this loader. An explicit
// additionalProperties, true or false, is left alone.
func StampStrict(schema map[string]any) {
	if _, declared := schema["additionalProperties"]; declared {
		return
	}
	if t, _ := schema["type"].(string); t == "object" || schema["properties"] != nil {
		schema["additionalProperties"] = false
	}
}

// bridgeDestructivePrefixes is the action-name heuristic basecamp-mcp-server
// used to classify destructive operations before the SDK behavior models
// carried a destructive trait. It is a bridge, not a home: an operation whose
// model declares "destructive" ignores the heuristic entirely. Products
// should carry a tripwire test that fails when their vendored model starts
// emitting the trait, so the bridge gets deleted rather than becoming
// permanent.
var bridgeDestructivePrefixes = []string{"delete", "trash", "remove", "destroy", "unsubscribe", "purge"}

// BridgeDestructive classifies an action name as destructive by prefix. See
// bridgeDestructivePrefixes for why this exists and when it dies.
func BridgeDestructive(action string) bool {
	for _, p := range bridgeDestructivePrefixes {
		if strings.HasPrefix(action, p) {
			return true
		}
	}
	return false
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
