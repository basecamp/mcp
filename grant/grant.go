// Package grant fits a server's tool surface to the OAuth grant a request
// carries. tools/list names only the tools the grant can run. A tools/call the
// grant can't run is answered in band: an isError result whose text says which
// permission the tool needs and that reconnecting asks the person for it, and
// whose _meta["mcp/www_authenticate"] carries a WWW-Authenticate challenge
// naming the granted scopes plus the needed ones. A host that relinks from a
// tool result (ChatGPT) acts on the challenge; any other host shows the text.
//
// There is no HTTP 403 step-up. Each server's 401 asks for the most the
// client may hold and the person narrows at consent, so a narrow grant is a
// choice; the tool result is where that choice is explained, not a protocol
// error a client may or may not act on.
//
// What a tool needs, what a grant implies and what the refusal says are the
// product's; this package holds the listing filter, the refusal's shape and
// the challenge's syntax, the parts that must agree across servers.
package grant

import (
	"context"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MetaWWWAuthenticate is the tool-result _meta key a host reads a challenge
// from, as OpenAI's Apps SDK defines it: an array of WWW-Authenticate values.
const MetaWWWAuthenticate = "mcp/www_authenticate"

// Policy is one server's scope rules.
type Policy struct {
	// Needs is the scopes a tool needs, every one of them. ok false means the
	// policy doesn't govern the tool: it is listed and runs whatever the
	// grant, and a name the server doesn't serve still reaches the SDK's own
	// unknown-tool answer.
	Needs func(tool string) (needs []string, ok bool)
	// Grant is the scope the request's grant holds, space-delimited as the
	// authorization server issued it, and whether the request is scoped at
	// all. An unscoped request (the person's own session, on stdio) is listed
	// and runs every tool. A scoped request whose scope is unknown should be
	// given the narrowest scope the product has: the server mustn't widen
	// what it can't see.
	Grant func(ctx context.Context, req mcp.Request) (scope string, scoped bool)
	// Implies is what each scope grants beyond itself, as the authorization
	// server has it (HEY: full implies read, write and send).
	Implies map[string][]string
	// Refuse renders the answer to a call the grant can't run, usually
	// Refusal with the product's sentence and Challenge.
	Refuse func(ctx context.Context, s Shortfall) *mcp.CallToolResult
}

// Shortfall is a call the grant can't run.
type Shortfall struct {
	Tool string
	// Granted is the grant's scope as issued.
	Granted []string
	// Missing is what Tool needs that Granted doesn't imply, in Needs' order.
	Missing []string
	// Ask is Granted then Missing, each once: the whole set a challenge asks
	// for, since a host may replace the scopes it holds rather than add to
	// them.
	Ask []string
}

// Expand is everything scope grants under implies, each name once, in the
// order first reached.
func Expand(implies map[string][]string, scope []string) []string {
	var out []string
	var walk func(string)
	walk = func(s string) {
		if slices.Contains(out, s) {
			return
		}
		out = append(out, s)
		for _, implied := range implies[s] {
			walk(implied)
		}
	}
	for _, s := range scope {
		walk(s)
	}
	return out
}

// missing is what needs asks for that granted doesn't imply.
func (p Policy) missing(granted, needs []string) []string {
	have := Expand(p.Implies, granted)
	var out []string
	for _, n := range needs {
		if !slices.Contains(have, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// Shortfall reports what the request's grant lacks for tool, and false when
// the grant can run it (the request is unscoped, the policy doesn't govern
// the tool, or the grant implies every scope it needs).
func (p Policy) Shortfall(ctx context.Context, req mcp.Request, tool string) (Shortfall, bool) {
	needs, governed := p.Needs(tool)
	if !governed {
		return Shortfall{}, false
	}
	scope, scoped := p.Grant(ctx, req)
	if !scoped {
		return Shortfall{}, false
	}
	granted := strings.Fields(scope)
	missing := p.missing(granted, needs)
	if len(missing) == 0 {
		return Shortfall{}, false
	}
	ask := slices.Clone(granted)
	for _, m := range missing {
		if !slices.Contains(ask, m) {
			ask = append(ask, m)
		}
	}
	return Shortfall{Tool: tool, Granted: granted, Missing: missing, Ask: ask}, true
}

// Middleware lists only the tools the grant can run and refuses, before the
// tool's handler, a call it can't. The listed result is copied, never changed
// in place; a page with nothing left still lists "tools": [].
//
// The refusal holds for a host that cached an older list, or calls a tool by
// name without listing, which is why it is not enough to filter the list.
func (p Policy) Middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
				if s, short := p.Shortfall(ctx, req, call.Params.Name); short {
					return p.Refuse(ctx, s), nil
				}
				return next(ctx, method, req)
			}
			res, err := next(ctx, method, req)
			list, ok := res.(*mcp.ListToolsResult)
			if _, isList := req.(*mcp.ListToolsRequest); err != nil || !ok || !isList || list == nil {
				return res, err
			}
			filtered := *list
			filtered.Tools = make([]*mcp.Tool, 0, len(list.Tools))
			for _, t := range list.Tools {
				if _, short := p.Shortfall(ctx, req, t.Name); !short {
					filtered.Tools = append(filtered.Tools, t)
				}
			}
			return &filtered, nil
		}
	}
}

// Challenge is the WWW-Authenticate value a refusal carries: RFC 6750's
// insufficient_scope, the scopes to ask for, the protected resource metadata
// URL (RFC 9728) the host authorizes against, and a description, which
// OpenAI's Apps SDK requires alongside the error. Quoted values are escaped
// per RFC 9110 §5.6.4, so no input can end a parameter early, and the
// description keeps only the characters RFC 6750 §3 allows it (printable
// ASCII but `"` and `\`); any other becomes a space, so a strict client
// doesn't reject the challenge. The refusal's text keeps the full sentence.
func Challenge(resourceMetadata string, scopes []string, description string) string {
	return `Bearer error="insufficient_scope", scope=` + quote(strings.Join(scopes, " ")) +
		`, resource_metadata=` + quote(resourceMetadata) +
		`, error_description=` + quote(errorDescription(description))
}

// errorDescription maps every character RFC 6750 §3 forbids in
// error_description (outside %x20-21 / %x23-5B / %x5D-7E) to a space.
func errorDescription(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return ' '
		}
		return r
	}, s)
}

// quote renders s as an RFC 9110 quoted-string. Control characters, which a
// quoted-string can't carry, become spaces.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Refusal is the in-band answer to a call the grant can't run: isError, the
// product's sentence as the one text content, and challenge under
// MetaWWWAuthenticate as a one-element array.
func Refusal(text, challenge string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		Meta:    mcp.Meta{MetaWWWAuthenticate: []string{challenge}},
	}
}
