// Package gateway implements the domain-gateway dispatch conventions shared
// by our MCP servers: one MCP tool per domain, the {"action", "params"}
// calling convention, an in-band describe action serving per-operation
// schemas, in-band isError failures (never protocol errors), read-only
// filtering, fail-closed domain narrowing, and derived tool annotations
// (title and explicit safety hints on every tool, computed from the served
// surface after filtering — never hardcoded).
//
// Extracted from hey-mcp-server's server package and basecamp-mcp-server's
// domain registry, where the conventions existed by duplication. The gateway
// serves any catalog that implements the small Domain interface; what an
// action actually does when dispatched is the product's Handler.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DescribeAction is the reserved gateway action serving per-operation
// schemas. Catalogs must not register an operation under this name.
const DescribeAction = "describe"

// Operation is the dispatch-relevant surface of one catalog operation.
type Operation struct {
	// Action is the gateway action name, e.g. "create_calendar_todo".
	Action string
	// ReadOnly reports whether the operation only reads data. Write
	// operations are refused when the server runs read-only.
	ReadOnly bool
}

// Domain is the catalog surface the gateway serves: a curated group of
// operations exposed as a single MCP tool with action dispatch.
type Domain interface {
	// Name is the short domain key used for narrowing and error messages,
	// e.g. "todos".
	Name() string
	// ToolName is the MCP tool name, e.g. "hey_todos".
	ToolName() string
	// ToolTitle is the human-readable tool title for annotations. Required:
	// connector directories reject tools without one.
	ToolTitle() string
	// Description renders the tool description shown on tools/list.
	Description() string
	// InputSchema renders the JSON Schema for the tool's arguments.
	InputSchema() map[string]any
	// AllReadOnly reports whether every operation in the domain is read-only.
	AllReadOnly() bool
	// AllIdempotent reports whether every write operation is idempotent.
	// Read operations count as idempotent regardless of their traits, so an
	// all-read domain reports true.
	AllIdempotent() bool
	// AnyDestructive reports whether any operation deletes or irreversibly
	// alters data. Never true for an all-read domain.
	AnyDestructive() bool
	// ActionNames returns the domain's action names in sorted order.
	ActionNames() []string
	// Find returns the operation registered under the given action name.
	Find(action string) (Operation, bool)
	// Describe returns the describe payload for one action, or for the whole
	// domain when action is empty.
	Describe(action string) (any, error)
	// FilterReadOnly returns a copy of the domain containing only read-only
	// operations, reporting false when none remain.
	FilterReadOnly() (Domain, bool)
}

// Handler executes one dispatched action. It is the product's half of the
// gateway: the conventions above route the call, the handler performs it.
// Failures the caller can act on should be in-band ErrorResults; returned
// errors are protocol-level.
type Handler func(ctx context.Context, domain Domain, op Operation, params map[string]any) (*mcp.CallToolResult, error)

// Config selects the served tool surface.
type Config struct {
	// ReadOnly drops every write action from the catalog and refuses write
	// dispatch outright.
	ReadOnly bool
	// Domains narrows the served domains by name ("boxes", "search", ...).
	// Empty means all. Unknown names are a startup error — fail closed.
	Domains []string
	// Handler executes dispatched actions. Required.
	Handler Handler
}

// Server holds the catalog domains filtered by config.
type Server struct {
	cfg     Config
	domains []Domain
}

// New applies the config's domain narrowing and read-only filters.
func New(domains []Domain, cfg Config) (*Server, error) {
	if cfg.Handler == nil {
		return nil, fmt.Errorf("gateway: Config.Handler is required")
	}

	byName := map[string]Domain{}
	byTool := map[string]string{}
	for _, d := range domains {
		if _, ok := d.Find(DescribeAction); ok {
			return nil, fmt.Errorf("domain %q registers an operation named %q, which is reserved for the gateway describe action", d.Name(), DescribeAction)
		}
		if strings.TrimSpace(d.ToolTitle()) == "" {
			return nil, fmt.Errorf("domain %q has no tool title (connector directories require a title on every tool)", d.Name())
		}
		if _, dup := byName[d.Name()]; dup {
			// Silent overwrites would drop a domain's operations — a split
			// write half colliding with a literal "<key>_write" domain, say.
			return nil, fmt.Errorf("duplicate domain %q", d.Name())
		}
		if prev, dup := byTool[d.ToolName()]; dup {
			return nil, fmt.Errorf("domains %q and %q both serve tool %q", prev, d.Name(), d.ToolName())
		}
		byName[d.Name()] = d
		byTool[d.ToolName()] = d.Name()
	}

	names := cfg.Domains
	if len(names) == 0 {
		for _, d := range domains {
			names = append(names, d.Name())
		}
	}

	var served []Domain
	for _, name := range names {
		d, ok := byName[name]
		if !ok {
			known := make([]string, 0, len(byName))
			for k := range byName {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("unknown domain %q (known: %s)", name, strings.Join(known, ", "))
		}
		if cfg.ReadOnly {
			if d, ok = d.FilterReadOnly(); !ok {
				continue
			}
		}
		served = append(served, d)
	}

	return &Server{cfg: cfg, domains: served}, nil
}

// Domains returns the served domains.
func (s *Server) Domains() []Domain {
	return s.domains
}

// Overview renders the whole served surface in one payload: every domain's
// tool name and action summaries. Products can mount it on a meta tool as a
// one-call orientation point. It aggregates the same describe payloads the
// domain tools serve, after read-only filtering and domain narrowing, so it
// is honest by construction: an action outside the deployed surface never
// appears.
func (s *Server) Overview() (any, error) {
	type entry struct {
		Tool   string `json:"tool"`
		Title  string `json:"title"`
		Domain any    `json:"domain"`
	}
	overview := make([]entry, 0, len(s.domains))
	for _, d := range s.domains {
		payload, err := d.Describe("")
		if err != nil {
			return nil, fmt.Errorf("domain %q: %w", d.Name(), err)
		}
		overview = append(overview, entry{Tool: d.ToolName(), Title: d.ToolTitle(), Domain: payload})
	}
	return map[string]any{"domains": overview}, nil
}

func ptrBool(b bool) *bool {
	return &b
}

// BuildMCPServer constructs the SDK MCP server with one gateway tool per
// served domain. impl identifies the product server in the MCP initialize
// handshake.
func (s *Server) BuildMCPServer(impl *mcp.Implementation, logger *slog.Logger) *mcp.Server {
	mcpServer := mcp.NewServer(impl, &mcp.ServerOptions{Logger: logger})

	for _, domain := range s.domains {
		d := domain
		// Annotations are computed after New's read-only filtering, so they
		// describe exactly the served surface. DestructiveHint is tri-state
		// with an absent-means-true spec default, so it is always explicit:
		// false on clean and read-only tools (never true alongside
		// ReadOnlyHint — clients resolve that contradiction differently),
		// true only when a served write action is destructive. OpenWorldHint
		// is false: a product API is a closed world.
		readOnly := d.AllReadOnly()
		destructive := !readOnly && d.AnyDestructive()
		tool := &mcp.Tool{
			Name:        d.ToolName(),
			Description: d.Description(),
			InputSchema: d.InputSchema(),
			Annotations: &mcp.ToolAnnotations{
				Title:           d.ToolTitle(),
				ReadOnlyHint:    readOnly,
				IdempotentHint:  d.AllIdempotent(),
				DestructiveHint: &destructive,
				OpenWorldHint:   ptrBool(false),
			},
		}
		mcpServer.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.dispatch(ctx, d, req)
		})
	}

	return mcpServer
}

// arguments is the gateway calling convention every domain tool shares.
type arguments struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

// dispatch routes one tool call: describe serves catalog metadata, anything
// else resolves to a catalog operation and runs the product handler.
// Failures are in-band isError results per MCP convention, never protocol
// errors.
func (s *Server) dispatch(ctx context.Context, d Domain, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args arguments
	if err := remarshal(req.Params.Arguments, &args); err != nil {
		return ErrorResult("invalid arguments: %v", err), nil
	}

	if args.Action == DescribeAction {
		var target string
		if raw, present := args.Params["action"]; present {
			str, ok := raw.(string)
			if !ok {
				return ErrorResult("invalid arguments: params.action must be a string, got %T", raw), nil
			}
			target = str
		}
		payload, err := d.Describe(target)
		if err != nil {
			return ErrorResult("%v", err), nil
		}
		return JSONResult(payload)
	}

	op, ok := d.Find(args.Action)
	if !ok {
		return ErrorResult("unknown action %q in domain %q (actions: %s)",
			args.Action, d.Name(), strings.Join(append(d.ActionNames(), DescribeAction), ", ")), nil
	}
	if s.cfg.ReadOnly && !op.ReadOnly {
		return ErrorResult("action %q is not available in read-only mode", op.Action), nil
	}

	return s.cfg.Handler(ctx, d, op, args.Params)
}

// ErrorResult renders an in-band isError result.
func ErrorResult(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}},
	}
}

// JSONResult renders a value as an indented JSON text result.
func JSONResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrorResult("internal error: encode result"), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil
}

func remarshal(from, to any) error {
	data, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, to)
}
