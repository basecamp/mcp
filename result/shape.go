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
// or a primitive, so a list tool that answered with a bare array was
// unusable to them. An object is valid under every revision, so results are
// shaped as objects regardless of the negotiated version: one shape per tool,
// whichever client asks.
//
// A top-level array is served as {"items": [...]} — the key the size cap
// uses for a cut bare array. Any other non-object (a primitive, or null from a nil pointer)
// is served as {"value": ...}. The text content restates the same object.
//
// A nil slice at the top is still a list: it is served as {"items": []},
// never as {"value": null}. Whether a list is nil or empty in the first place
// is the producer's business, so nested values are marshalled as they come.

// ItemsKey wraps a top-level array; ValueKey wraps any other non-object.
const (
	ItemsKey = "items"
	ValueKey = "value"
)

// JSON renders v as a successful tool result: a text block and a
// structuredContent restating the same JSON object, a non-object wrapped as
// Marshal wraps it. StructuredContent is the already-marshalled bytes as a
// json.RawMessage, so the wire marshal embeds them verbatim and the text and
// structured copy can never diverge.
func JSON(v any) *mcp.CallToolResult {
	data, err := Marshal(v, true)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "internal error: encode result"}},
		}
	}
	result := Text(string(data))
	result.StructuredContent = json.RawMessage(data)
	return result
}

// Text renders a successful plain-text tool result.
func Text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// Marshal renders v as a tool result's JSON, a non-object wrapped as
// an object. indent selects JSON's indented text over the compact form
// the size cap measures.
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

// Object is the last word on a result's shape, applied to every result a
// server serves (Middleware applies it): a structuredContent that is not a
// JSON object — from a handler that set it without JSON — is wrapped as
// Marshal would wrap it, and every text block restating it is rewritten to
// match, so the two never disagree. Typed payloads that are already objects
// (a product's typed error payloads, say) keep their Go type, which a
// protocol layer may type-assert on. A nil content array becomes an empty
// one: the SDK would send it as null, and clients require the array. The
// result passed in is never modified.
func Object(result *mcp.CallToolResult) *mcp.CallToolResult {
	if result == nil {
		return nil
	}
	sc := result.StructuredContent
	if sc == nil {
		return withContentArray(result)
	}
	if raw, ok := sc.(json.RawMessage); ok && isJSONObject(raw) {
		return withContentArray(result)
	}
	data, err := json.Marshal(sc)
	if err != nil || isJSONObject(data) {
		return withContentArray(result)
	}
	// Both wrapped forms are built from this one marshal, so a handler's
	// json.Marshaler runs once and can't hand the text and the structured
	// form different values.
	payload := data
	if isNilSlice(sc) {
		payload = []byte("[]")
	}
	wrapped, err := json.Marshal(map[string]json.RawMessage{wrapKey(payload): payload})
	if err != nil {
		return withContentArray(result)
	}
	out := *result
	out.StructuredContent = json.RawMessage(wrapped)
	// A fresh, never-nil slice: the copy must not write through to the
	// handler's, and a structured-only result keeps its content array.
	out.Content = make([]mcp.Content, len(result.Content))
	copy(out.Content, result.Content)
	var indented bytes.Buffer
	textErr := json.Indent(&indented, wrapped, "", "  ")
	text := indented.Bytes()
	for i, c := range out.Content {
		// Every block that restates the value is rewritten, not just the
		// first text block: prose may come first.
		if tc, ok := c.(*mcp.TextContent); ok && textErr == nil && sameJSON([]byte(tc.Text), data) {
			out.Content[i] = &mcp.TextContent{Text: string(text), Meta: tc.Meta, Annotations: tc.Annotations}
		}
	}
	return &out
}

// withContentArray returns result with a non-nil content array: a copy when
// the handler left it nil.
func withContentArray(result *mcp.CallToolResult) *mcp.CallToolResult {
	if result.Content != nil {
		return result
	}
	out := *result
	out.Content = []mcp.Content{}
	return &out
}

// sameJSON reports whether a and b are the same JSON value: whitespace and
// object member order don't matter, and numbers compare by their text.
func sameJSON(a, b []byte) bool {
	va, okA := decodeJSON(a)
	vb, okB := decodeJSON(b)
	return okA && okB && reflect.DeepEqual(va, vb)
}

// isNilSlice reports whether v is a nil slice (other than []byte, which
// encodes as a string), which encodes as null but is a list all the same.
func isNilSlice(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Slice && rv.IsNil() && rv.Type().Elem().Kind() != reflect.Uint8
}
