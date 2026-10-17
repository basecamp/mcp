// Package result is how the basecamp MCP servers shape and size a tool
// result on its way out: structuredContent is always a JSON object (Object),
// JSON values are served as text plus that object (JSON), and a result over
// the server's size limit is cut where the cut can be explained (Cap).
// Middleware applies both to every tools/call result, so a server built on
// the gateway, which has no per-tool registry hook, gets the same posture as
// one that has.
//
// Extracted from basecamp-mcp-server's internal/tools (result_shape.go,
// result_cap.go), with hey-mcp-server's fix that a structured-only result
// keeps an empty, never-null content array.
package result

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Result shape: every structuredContent a server emits is a JSON object.
//
// The MCP schema typed structuredContent as an object ({ [key: string]:
// unknown }) through 2025-11-25; only 2026-07-28 (SEP-2106) widens it to any
// JSON value. Clients negotiating the older revisions — the official Python
// SDK among them — reject a whole result whose structuredContent is an array
// or a primitive. An object is valid under every revision, so results are
// shaped as objects regardless of the negotiated version: one shape per tool,
// whichever client asks.
//
// A top-level array is served as {"items": [...]} — the key the size cap
// uses for a cut bare array. Any other non-object (a primitive, or null from
// a nil pointer) is served as {"value": ...}. The text content restates the
// same object.
//
// A nil slice at the top is still a list: it is served as {"items": []},
// never as {"value": null}. Nested values are marshalled as they come.

// ItemsKey wraps a top-level array; ValueKey wraps any other non-object.
const (
	ItemsKey = "items"
	ValueKey = "value"
)

// JSON renders v as a successful result: indented JSON text, restated as
// structuredContent, a non-object wrapped as an object.
func JSON(v any) *mcp.CallToolResult {
	data, err := Marshal(v, true)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "internal error: encode result"}},
		}
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
		StructuredContent: json.RawMessage(data),
	}
}

// Marshal renders v as a tool result's JSON, a non-object wrapped as an
// object. indent selects JSON's indented text over the compact form the size
// cap measures.
func Marshal(v any, indent bool) ([]byte, error) {
	marshal := json.Marshal
	if indent {
		marshal = func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
	}
	if isNilSlice(v) {
		return marshal(map[string][]any{ItemsKey: {}})
	}
	data, err := marshal(v)
	if err != nil {
		return nil, err
	}
	if isJSONObject(data) {
		return data, nil
	}
	return marshal(map[string]json.RawMessage{wrapKey(data): data})
}

// wrapKey names the key a non-object JSON value is served under.
func wrapKey(data []byte) string {
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		return ItemsKey
	}
	return ValueKey
}

// isJSONObject reports whether data is a JSON object (ignoring whitespace).
func isJSONObject(data []byte) bool {
	t := bytes.TrimSpace(data)
	return len(t) > 0 && t[0] == '{'
}

// Object is the last word on a result's shape: a structuredContent that is
// not a JSON object — from a handler that set it without JSON — is wrapped as
// Marshal would wrap it, and a text block restating it is rewritten to match,
// so the two never disagree. Typed payloads that are already objects keep
// their Go type, which a protocol layer may type-assert on. The result passed
// in is never modified.
func Object(result *mcp.CallToolResult) *mcp.CallToolResult {
	if result == nil || result.StructuredContent == nil {
		return result
	}
	sc := result.StructuredContent
	if raw, ok := sc.(json.RawMessage); ok && isJSONObject(raw) {
		return result
	}
	data, err := json.Marshal(sc)
	if err != nil || isJSONObject(data) {
		return result
	}
	wrapped, err := Marshal(sc, false)
	if err != nil {
		return result
	}
	out := *result
	out.StructuredContent = json.RawMessage(wrapped)
	// A fresh, never-nil slice: older clients require the content array, and
	// a structured-only result must not lose it to the copy.
	out.Content = make([]mcp.Content, len(result.Content))
	copy(out.Content, result.Content)
	for i, c := range out.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		if sameJSON([]byte(tc.Text), data) {
			if text, err := Marshal(sc, true); err == nil {
				out.Content[i] = &mcp.TextContent{Text: string(text), Meta: tc.Meta, Annotations: tc.Annotations}
			}
		}
		break
	}
	return &out
}

// sameJSON reports whether a and b are the same JSON text up to whitespace.
func sameJSON(a, b []byte) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}

// isNilSlice reports whether v is a nil slice (other than []byte, which
// encodes as a string), which encodes as null but is a list all the same.
func isNilSlice(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Slice && rv.IsNil() && rv.Type().Elem().Kind() != reflect.Uint8
}
