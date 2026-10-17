package result

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Middleware is receiving middleware that serves every tools/call result as
// Object, then Cap(maxBytes), so the shape and size rules hold for whatever a
// handler returns, on any transport. maxBytes <= 0 disables the cap (the
// shape rule still applies); a positive value below MinBytes is raised to it.
func Middleware(maxBytes int) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			if call, ok := res.(*mcp.CallToolResult); ok {
				return Cap(Object(call), maxBytes), nil
			}
			return res, nil
		}
	}
}
