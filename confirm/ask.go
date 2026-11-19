package confirm

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport is how clients reach the server, which decides whether a
// legacy-protocol client can be asked anything.
type Transport string

const (
	// TransportStdio is one local session: the SDK can deliver a legacy
	// elicitation on it.
	TransportStdio Transport = "stdio"
	// TransportHTTP is stateless Streamable HTTP: only a 2026-07-28 client,
	// which takes input_required results, can be asked.
	TransportHTTP Transport = "http"
)

// ProtocolMRTR is the first protocol revision with input_required results.
const ProtocolMRTR = "2026-07-28"

// ClientRequest is what any request says about its client: a 2026-07-28
// request carries it in _meta, an older one has it from the session's
// initialize. Every *mcp.ServerRequest is one.
type ClientRequest interface {
	ProtocolVersion() string
	ClientCapabilities() *mcp.ClientCapabilities
}

// CanAsk reports whether the client a request comes from can answer a form:
// a 2026-07-28 client that declares form elicitation (or an empty
// elicitation object, which means form, as the SDK reads it) can on any
// transport; a legacy client only on stdio, with elicitation declared;
// legacy stateless HTTP never.
func CanAsk(req ClientRequest, transport Transport) bool {
	if req == nil {
		return false
	}
	caps := req.ClientCapabilities()
	form := caps != nil && caps.Elicitation != nil && (caps.Elicitation.Form != nil || caps.Elicitation.URL == nil)
	if req.ProtocolVersion() >= ProtocolMRTR {
		return form
	}
	return transport == TransportStdio && form
}

// Field is how the form's one boolean, confirm, reads to the person.
type Field struct {
	Title       string
	Description string
}

// Elicitation is the question: a form with one required boolean, confirm,
// and the message the host shows above it. Every field in the message a
// sender or the model chose should be rendered inert by the caller.
func Elicitation(message string, f Field) *mcp.ElicitParams {
	return &mcp.ElicitParams{
		Mode:    "form",
		Message: message,
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{
					"type":        "boolean",
					"title":       f.Title,
					"description": f.Description,
				},
			},
			"required": []string{"confirm"},
		},
	}
}

// Answer reads the person's answer to the question keyed inputID: true only
// for an accepted form with confirm: true. Decline, cancel and confirm: false
// are all a "no"; an answer that is missing or malformed is an error.
func Answer(responses mcp.InputResponseMap, inputID string) (bool, error) {
	raw, ok := responses[inputID]
	if !ok || raw == nil {
		return false, errors.New("the confirmation's answer is missing")
	}
	answer, ok := raw.(*mcp.ElicitResult)
	if !ok || answer == nil {
		return false, errors.New("the confirmation's answer isn't a form answer")
	}
	switch answer.Action {
	case "decline", "cancel":
		return false, nil
	case "accept":
	default:
		return false, fmt.Errorf("the confirmation's answer %q isn't accept, decline or cancel", answer.Action)
	}
	yes, ok := answer.Content["confirm"].(bool)
	if !ok {
		return false, errors.New("the confirmation's answer has no yes or no")
	}
	return yes, nil
}

// FilterTools keeps out of tools/list the tools a client isn't listed, so
// none is offered to a model that would only be turned away: listed answers,
// for each tools/list request, which names it lists. The result is copied,
// never changed in place.
//
// Only the list changes. A call still runs as it did, for a client whose
// list is cached, so a tool that asks must still refuse a client that can't
// answer.
func FilterTools(listed func(req *mcp.ListToolsRequest) func(name string) bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			list, ok := res.(*mcp.ListToolsResult)
			listReq, isList := req.(*mcp.ListToolsRequest)
			if err != nil || !ok || !isList || list == nil {
				return res, err
			}
			keep := listed(listReq)
			filtered := *list
			filtered.Tools = nil
			for _, t := range list.Tools {
				if keep(t.Name) {
					filtered.Tools = append(filtered.Tools, t)
				}
			}
			return &filtered, nil
		}
	}
}
