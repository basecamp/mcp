// Package requestlog writes the one line per MCP message a hosted server
// answers ("mcp request"): which method and tool, who asked (the client's
// name and version, the protocol revision, the grant), how long it took and
// how it ended, and the request id that joins it to the HTTP request it rode
// in on and to the calls the server made for it.
//
// It never logs what a message carries: no arguments, no tool input or
// output, no error text (a tool's error can quote the user's content), no
// token. A tools/call names its tool only when it is one the server serves,
// so a client can't write a name of its own into the log, and every string a
// client chooses (client name and version, User-Agent, request id) is cut to
// MaxField bytes.
//
// Extracted from hey-mcp-server and fizzy-mcp-server internal/hosted
// (requestlog.go), which logged the same line; the request id is new here.
package requestlog

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Message is the line's slog message.
const Message = "mcp request"

// MaxField bounds each client-chosen string the line carries, so a client
// can't fill a line.
const MaxField = 128

// UnknownTool is what a tools/call naming a tool the server doesn't serve
// logs as its tool.
const UnknownTool = "(unknown)"

// Options are what a server plugs into the line.
type Options struct {
	// Tools are the tools the server serves.
	Tools []string
	// Grant names the grant a request rides on, or "" when it isn't known.
	// The line has no grant when Grant is nil or answers "".
	Grant func(context.Context) string
	// CutShort reports a request cut short other than by its context's
	// cancellation or deadline: a request budget that isn't a deadline on
	// the context, say. A tool cut short answers with an error result and no
	// error, so without this the line would say tool_error. Optional.
	CutShort func(context.Context) bool
}

// Install has server log one line per MCP message it receives. Call it last,
// after every other receiving middleware, so the line times all of them.
func Install(server *mcp.Server, logger *slog.Logger, opts Options) {
	server.AddReceivingMiddleware(Middleware(logger, opts))
}

// Middleware writes one line per MCP message the server receives, once it is
// answered. Install it outermost, so the duration covers every other
// middleware and the handler.
func Middleware(logger *slog.Logger, opts Options) mcp.Middleware {
	tools := make(map[string]bool, len(opts.Tools))
	for _, name := range opts.Tools {
		tools[name] = true
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			logger.LogAttrs(ctx, slog.LevelInfo, Message,
				append(requestAttrs(ctx, method, req, tools, opts.Grant),
					slog.Int64("duration_ms", time.Since(start).Milliseconds()),
					slog.Group("result", outcomeAttrs(ctx, res, err, opts.CutShort)...))...)
			return res, err
		}
	}
}

// clientDescriber is what any server request says about its client: from
// the request's own _meta under 2026-07-28, else from the session's
// initialize, which a stateless server's later requests don't have.
type clientDescriber interface {
	ProtocolVersion() string
	ClientInfo() *mcp.Implementation
}

func requestAttrs(ctx context.Context, method string, req mcp.Request, tools map[string]bool, grant func(context.Context) string) []slog.Attr {
	attrs := []slog.Attr{slog.String("method", method)}
	protocol, client := "", (*mcp.Implementation)(nil)
	if d, ok := req.(clientDescriber); ok {
		protocol, client = d.ProtocolVersion(), d.ClientInfo()
	}
	// The initialize request names its client and revision in its params,
	// before the session has recorded either.
	if init, ok := req.(*mcp.InitializeRequest); ok && mcp.HasParams(init) {
		protocol, client = init.Params.ProtocolVersion, init.Params.ClientInfo
	}
	attrs = append(attrs, slog.String("protocol", truncate(protocol)))
	if client != nil {
		attrs = append(attrs, slog.String("client", truncate(client.Name)), slog.String("client_version", truncate(client.Version)))
	}
	if call, ok := req.(*mcp.CallToolRequest); ok && mcp.HasParams(call) {
		name := call.Params.Name
		if !tools[name] {
			name = UnknownTool
		}
		attrs = append(attrs, slog.String("tool", name))
	}
	// A stateless server keeps no sessions, so what tells clients apart is
	// whether one sent an Mcp-Session-Id anyway, believing it holds a
	// session.
	session := "none"
	var header http.Header
	if extra := req.GetExtra(); extra != nil {
		header = extra.Header
	}
	if header != nil {
		if header.Get("Mcp-Session-Id") != "" {
			session = "sent"
		}
		if ua := header.Get("User-Agent"); ua != "" {
			attrs = append(attrs, slog.String("user_agent", truncate(ua)))
		}
	}
	attrs = append(attrs, slog.String("session", session))
	if grant != nil {
		if id := grant(ctx); id != "" {
			attrs = append(attrs, slog.String("grant", id))
		}
	}
	id := RequestID(ctx)
	if id == "" && header != nil {
		id = truncate(header.Get(Header))
	}
	if id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	return attrs
}

// outcomeAttrs says how a message ended: canceled (the client cancelled or
// went away, a deadline passed, or the server cut it short), ok, a tool's
// own error result (tool_error), a question back to the client
// (input_required), or a protocol error with its JSON-RPC code. Never the
// error's text.
//
// Cancellation comes first: a tool whose upstream call is cut short answers
// with an error result and no error, so only the request's context (or
// cutShort) tells it from a tool that failed on its own.
func outcomeAttrs(ctx context.Context, res mcp.Result, err error, cutShort func(context.Context) bool) []any {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (cutShort != nil && cutShort(ctx)) {
		return []any{slog.String("status", "canceled")}
	}
	if err != nil {
		var wire *jsonrpc.Error
		if errors.As(err, &wire) {
			return []any{slog.String("status", "error"), slog.Int64("code", wire.Code)}
		}
		return []any{slog.String("status", "error")}
	}
	if call, ok := res.(*mcp.CallToolResult); ok && call != nil {
		switch {
		// The SDK labels a result input_required (NeedsInput) only after
		// every middleware has returned, so this asks what it asks: the
		// same test the SDK's label applies.
		case call.InputRequests != nil || call.RequestState != "":
			return []any{slog.String("status", "input_required")}
		case call.IsError:
			return []any{slog.String("status", "tool_error")}
		}
	}
	return []any{slog.String("status", "ok")}
}

func truncate(s string) string {
	if len(s) <= MaxField {
		return s
	}
	return strings.ToValidUTF8(s[:MaxField], "")
}
