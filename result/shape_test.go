package result

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestObject covers the last word on a result's shape: whatever a handler
// set, structuredContent leaves as an object, a text block that restated it
// is rewritten to match, and the content array is never null.
func TestObject(t *testing.T) {
	t.Run("typed array, restated in text", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "[1, 2]"}},
			StructuredContent: []int{1, 2},
		})
		assertStructured(t, out, `{"items": [1, 2]}`)
		assert.JSONEq(t, `{"items": [1, 2]}`, resultText(t, out))
	})
	t.Run("raw array beside prose", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "Two things."}},
			StructuredContent: json.RawMessage(`[1,2]`),
		})
		assertStructured(t, out, `{"items": [1, 2]}`)
		assert.Equal(t, "Two things.", resultText(t, out))
	})
	t.Run("restated after prose", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Two things."},
				&mcp.TextContent{Text: "[1,2]"},
			},
			StructuredContent: json.RawMessage(`[1,2]`),
		})
		assertStructured(t, out, `{"items": [1, 2]}`)
		assert.Equal(t, "Two things.", resultText(t, out))
		assert.JSONEq(t, `{"items": [1, 2]}`, out.Content[1].(*mcp.TextContent).Text)
	})
	t.Run("nil slice", func(t *testing.T) {
		var none []string
		out := Object(&mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "null"}},
			StructuredContent: none,
		})
		assertStructured(t, out, `{"items": []}`)
		assert.JSONEq(t, `{"items": []}`, resultText(t, out))
	})
	t.Run("primitive", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{StructuredContent: "done"})
		assertStructured(t, out, `{"value": "done"}`)
	})
	t.Run("typed object keeps its type", func(t *testing.T) {
		payload := typedPayload{Code: "not_found"}
		in := &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: payload, IsError: true}
		out := Object(in)
		assert.Same(t, in, out)
		_, ok := out.StructuredContent.(typedPayload)
		assert.True(t, ok)
	})
	t.Run("raw object untouched", func(t *testing.T) {
		in := JSON(map[string]int{"id": 1})
		assert.Same(t, in, Object(in))
	})
	t.Run("restated with object keys in another order", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: `[ {"b":2,"a":1} ]`}},
			StructuredContent: []map[string]int{{"a": 1, "b": 2}},
		})
		assertStructured(t, out, `{"items": [{"a": 1, "b": 2}]}`)
		assert.JSONEq(t, `{"items": [{"a": 1, "b": 2}]}`, resultText(t, out))
	})
	t.Run("raw null", func(t *testing.T) {
		assertStructured(t, Object(&mcp.CallToolResult{StructuredContent: json.RawMessage(`null`)}), `{"value": null}`)
	})
	t.Run("nil content becomes an empty array", func(t *testing.T) {
		for name, in := range map[string]*mcp.CallToolResult{
			"structured object": {StructuredContent: typedPayload{Code: "x"}},
			"structured array":  {StructuredContent: []int{1}},
			"empty result":      {},
		} {
			out := Object(in)
			assert.NotNil(t, out.Content, name)
			assert.Nil(t, in.Content, "%s: the handler's result is not modified", name)
			data, err := json.Marshal(out)
			require.NoError(t, err)
			assert.Contains(t, string(data), `"content":[]`, name)
		}
	})
	t.Run("a marshaler runs once, so text and structured agree", func(t *testing.T) {
		// The text restates what the marshaler's next call answers: [2].
		m := &countingMarshaler{calls: 1}
		out := Object(&mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: `[2]`}},
			StructuredContent: m,
		})
		assertStructured(t, out, `{"items": [2]}`)
		assert.JSONEq(t, `{"items": [2]}`, out.Content[0].(*mcp.TextContent).Text)
		assert.Equal(t, 2, m.calls, "Object marshals the value once")
	})
	t.Run("nil result", func(t *testing.T) {
		assert.Nil(t, Object(nil))
	})
}

// countingMarshaler answers a different list on every call, as a stateful
// marshaler might.
type countingMarshaler struct{ calls int }

func (m *countingMarshaler) MarshalJSON() ([]byte, error) {
	m.calls++
	return json.Marshal([]int{m.calls})
}

// typedPayload stands in for a product's typed error payload: an object a
// protocol layer type-asserts on.
type typedPayload struct {
	Code string `json:"code"`
}

func assertStructured(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(raw))
}

// TestCapResult_ShortensAServedList verifies that a list served as
// {"items": [...]} is still cut to its leading items, as a bare array was:
// nothing beside it can point past the cut, so it keeps whole items rather
// than falling back to a flagged prefix of its text.
func TestCapResult_ShortensAServedList(t *testing.T) {
	items := make([]map[string]any, 200)
	for i := range items {
		items[i] = map[string]any{"id": i, "name": "an item with some text to take up room"}
	}
	out := Cap(JSON(items), MinBytes)
	var payload struct {
		Items      []map[string]any `json:"items"`
		Truncated  bool             `json:"truncated"`
		Truncation struct {
			Field    string `json:"field"`
			Returned int    `json:"returned"`
			Total    int    `json:"total"`
		} `json:"truncation"`
		PartialJSON string `json:"partial_json"`
	}
	raw, err := json.Marshal(out.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &payload))
	assert.True(t, payload.Truncated)
	assert.Empty(t, payload.PartialJSON, "a served list fell back to a text prefix")
	assert.Equal(t, "items", payload.Truncation.Field)
	assert.Equal(t, 200, payload.Truncation.Total)
	assert.Equal(t, len(payload.Items), payload.Truncation.Returned)
	assert.Positive(t, payload.Truncation.Returned)
}
