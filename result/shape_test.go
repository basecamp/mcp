package result

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Whatever a handler set, structuredContent leaves as an object, and a text
// block that restated it is rewritten to match.
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
		assertStructured(t, Object(&mcp.CallToolResult{StructuredContent: "done"}), `{"value": "done"}`)
	})
	t.Run("raw null", func(t *testing.T) {
		assertStructured(t, Object(&mcp.CallToolResult{StructuredContent: json.RawMessage(`null`)}), `{"value": null}`)
	})
	t.Run("typed object keeps its type", func(t *testing.T) {
		type payload struct {
			ID int `json:"id"`
		}
		in := &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: payload{ID: 1}}
		out := Object(in)
		assert.Same(t, in, out)
		assert.IsType(t, payload{}, out.StructuredContent)
	})
	t.Run("object with nil content gets an empty content array", func(t *testing.T) {
		type payload struct {
			ID int `json:"id"`
		}
		in := &mcp.CallToolResult{StructuredContent: payload{ID: 1}}
		out := Object(in)
		assert.NotNil(t, out.Content)
		assert.Nil(t, in.Content, "the handler's result is not modified")
		assert.IsType(t, payload{}, out.StructuredContent)
	})
	t.Run("every restating text block is rewritten", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "Two things."}, &mcp.TextContent{Text: "[1,2]"}},
			StructuredContent: []int{1, 2},
		})
		assert.Equal(t, "Two things.", resultText(t, out))
		second, ok := out.Content[1].(*mcp.TextContent)
		require.True(t, ok)
		assert.JSONEq(t, `{"items": [1, 2]}`, second.Text)
	})
	t.Run("raw object untouched", func(t *testing.T) {
		in := JSON(map[string]int{"id": 1})
		assert.Same(t, in, Object(in))
	})
	t.Run("structured only keeps an empty content array", func(t *testing.T) {
		out := Object(&mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: []int{1}})
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"content":[]`)
	})
	t.Run("input not mutated", func(t *testing.T) {
		text := &mcp.TextContent{Text: "[1]"}
		in := &mcp.CallToolResult{Content: []mcp.Content{text}, StructuredContent: []int{1}}
		_ = Object(in)
		assert.Same(t, text, in.Content[0])
		assert.Equal(t, "[1]", text.Text)
		assert.Equal(t, []int{1}, in.StructuredContent)
	})
}

// JSON serves a list as the same object Object would make of it.
func TestJSONServesObjects(t *testing.T) {
	assertStructured(t, JSON([]int{1}), `{"items": [1]}`)
	assertStructured(t, JSON(3), `{"value": 3}`)
	assert.JSONEq(t, `{"items": [1]}`, resultText(t, JSON([]int{1})))
	assert.True(t, JSON(make(chan int)).IsError, "an unencodable value is an error result")
}

// A list served as {"items": [...]} is still cut to its leading items, as a
// bare array is: nothing beside it can point past the cut.
func TestCap_ShortensAServedList(t *testing.T) {
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

func assertStructured(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(raw))
}
