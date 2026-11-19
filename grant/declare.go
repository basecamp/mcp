package grant

import (
	"context"
	"maps"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MetaSecuritySchemes is the tool-descriptor _meta key a host reads a tool's
// auth policy from, as OpenAI's Apps SDK defines it. OpenAI's descriptor
// also has a top-level securitySchemes, which the MCP tool type has no field
// for; the Apps SDK reads the _meta mirror, and its own Go example declares
// the scheme there and nowhere else.
const MetaSecuritySchemes = "securitySchemes"

// OAuth2 is a securitySchemes value naming one scheme: the tool needs an OAuth
// 2.0 access token holding scopes. An empty scopes is still written, as [].
func OAuth2(scopes ...string) []any {
	if scopes == nil {
		scopes = []string{}
	}
	return []any{map[string]any{"type": "oauth2", "scopes": scopes}}
}

// Declare stamps each listed tool that scopes names with
// _meta.securitySchemes = OAuth2(scopes...): the scopes a client requests to
// run it, in the authorization server's own names. ok false leaves the tool
// as it is.
//
// ChatGPT relinks a connection from a tool result only for a tool that
// declares an oauth2 scheme: "Triggering the tool-level OAuth flow requires
// both metadata (securitySchemes and the resource metadata document) and
// runtime errors that carry _meta["mcp/www_authenticate"]. Without both halves
// ChatGPT will not show the linking UI for that tool."
// (https://developers.openai.com/plugins/build/auth#triggering-authentication-ui)
// So a server whose Policy refuses a call should declare every tool the
// policy governs.
//
// The declaration is what the tool needs, not what the request holds: it is
// the same for every grant, so the list a directory scans under one grant
// declares what every other grant is listed. The listed result and its tools
// are copied, never changed in place; a tool's other _meta keys are kept.
func Declare(scopes func(tool string) ([]string, bool)) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			list, ok := res.(*mcp.ListToolsResult)
			if _, isList := req.(*mcp.ListToolsRequest); err != nil || !ok || !isList || list == nil {
				return res, err
			}
			declared := *list
			declared.Tools = make([]*mcp.Tool, len(list.Tools))
			for i, t := range list.Tools {
				s, ok := scopes(t.Name)
				if !ok {
					declared.Tools[i] = t
					continue
				}
				stamped := *t
				stamped.Meta = maps.Clone(t.Meta)
				if stamped.Meta == nil {
					stamped.Meta = mcp.Meta{}
				}
				stamped.Meta[MetaSecuritySchemes] = OAuth2(s...)
				declared.Tools[i] = &stamped
			}
			return &declared, nil
		}
	}
}
