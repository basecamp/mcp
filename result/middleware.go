package result

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Middleware holds a server's wire shape as receiving middleware, so it
// applies to every transport and every handler behind the server, however the
// tools were built (a gateway, a flat catalog, hand-registered tools):
//
//   - tools/list: a tool's top-level title (MCP 2025-06+) and
//     annotations.title match; whichever is empty is filled from the other.
//     Clients that read the top-level title get the same name, and the
//     Claude connector directory, which reads annotations.title, keeps it.
//   - tools/call: structuredContent is always a JSON object
//     (ObjectStructuredContent), and a successful result is held to maxBytes
//     (Cap; maxBytes <= 0 disables the cap).
//
// Both rewrite what a handler returned after the SDK checked it against the
// tool's output schema, so a tool served behind this middleware declares no
// output schema, or an object one that admits the wrapping keys (items,
// value) and the cap's envelope (truncated, truncation, partial_json):
// not additionalProperties false. A product holds that with a test over its
// served tools, as hey-mcp-server does.
//
// Install it once: server.AddReceivingMiddleware(result.Middleware(n)).
// Extracted from hey-mcp-server's wireShape, itself basecamp-mcp-server's
// registry posture (#236, #237) applied where a toolkit-built server can see
// every tool and every result.
func Middleware(maxBytes int) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			switch r := res.(type) {
			case *mcp.ListToolsResult:
				return mirrorTitles(r), nil
			case *mcp.CallToolResult:
				return Cap(ObjectStructuredContent(r), maxBytes), nil
			}
			return res, nil
		}
	}
}

// mirrorTitles fills whichever of a tool's title and annotations.title is
// empty from the other. The listed tools are the server's own definitions,
// so each changed tool (and its annotations) is copied, never written
// through.
func mirrorTitles(result *mcp.ListToolsResult) *mcp.ListToolsResult {
	if result == nil {
		return nil
	}
	out := *result
	out.Tools = make([]*mcp.Tool, len(result.Tools))
	for i, tool := range result.Tools {
		out.Tools[i] = mirrorTitle(tool)
	}
	return &out
}

func mirrorTitle(tool *mcp.Tool) *mcp.Tool {
	if tool == nil {
		return nil
	}
	var annotated string
	if tool.Annotations != nil {
		annotated = tool.Annotations.Title
	}
	switch {
	case tool.Title == "" && annotated != "":
		t := *tool
		t.Title = annotated
		return &t
	case tool.Title != "" && annotated == "":
		t := *tool
		a := mcp.ToolAnnotations{}
		if tool.Annotations != nil {
			a = *tool.Annotations
		}
		a.Title = tool.Title
		t.Annotations = &a
		return &t
	}
	return tool
}
