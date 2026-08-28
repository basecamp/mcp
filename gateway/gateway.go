// Package gateway implements the domain-gateway dispatch conventions shared
// by our MCP servers: one MCP tool per domain, the {"action", "params"}
// calling convention, an in-band describe action serving per-operation
// schemas, in-band isError failures (never protocol errors), read-only
// filtering, and fail-closed domain narrowing.
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
	// Description renders the tool description shown on tools/list.
	Description() string
	// InputSchema renders the JSON Schema for the tool's arguments.
	InputSchema() map[string]any
	// AllReadOnly reports whether every operation in the domain is read-only.
	AllReadOnly() bool
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
	for _, d := range domains {
		byName[d.Name()] = d
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

// BuildMCPServer constructs the SDK MCP server with one gateway tool per
// served domain. impl identifies the product server in the MCP initialize
// handshake.
func (s *Server) BuildMCPServer(impl *mcp.Implementation, logger *slog.Logger) *mcp.Server {
	mcpServer := mcp.NewServer(impl, &mcp.ServerOptions{Logger: logger})

	for _, domain := range s.domains {
		d := domain
		tool := &mcp.Tool{
			Name:        d.ToolName(),
			Description: d.Description(),
			InputSchema: d.InputSchema(),
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:   d.AllReadOnly(),
				IdempotentHint: false,
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
