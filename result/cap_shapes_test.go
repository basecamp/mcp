package result

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// totalBytes is everything the cap bounds: every text block plus the
// structured copy.
func totalBytes(t *testing.T, r *mcp.CallToolResult) int {
	t.Helper()
	n := 0
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			n += len(tc.Text)
		}
	}
	if r.StructuredContent != nil {
		data, err := json.Marshal(r.StructuredContent)
		require.NoError(t, err)
		n += len(data)
	}
	return n
}

func structuredObject(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	data, err := json.Marshal(r.StructuredContent)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m), "structuredContent must stay an object: %s", data)
	return m
}

// Shapes JSON doesn't produce are held to the cap too, and structured
// content stays an object whatever the text beside it says.
func TestCap_OtherShapesStayWithinTheLimit(t *testing.T) {
	big := map[string]any{"items": bigItems(100)}
	cases := map[string]*mcp.CallToolResult{
		"structured only, empty content": {Content: []mcp.Content{}, StructuredContent: big},
		"structured beside prose":        {Content: []mcp.Content{&mcp.TextContent{Text: "Here they are."}}, StructuredContent: big},
		"structured beside a scalar":     {Content: []mcp.Content{&mcp.TextContent{Text: "42"}}, StructuredContent: big},
		"small text, then a large one":   {Content: []mcp.Content{&mcp.TextContent{Text: "ok"}, &mcp.TextContent{Text: strings.Repeat("a", 50_000)}}},
		"two large texts":                {Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("a", 30_000)}, &mcp.TextContent{Text: strings.Repeat("b", 30_000)}}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := Cap(in, 8_000)
			assert.LessOrEqual(t, totalBytes(t, out), 8_000)
			assert.NotNil(t, out.Content)
			if in.StructuredContent != nil {
				got := structuredObject(t, out)
				assert.Equal(t, true, got["truncated"], "a cut structured copy says so")
			}
		})
	}
}

// Prose beside a structured copy is kept as written when it fits.
func TestCap_ProseBesideStructuredIsKept(t *testing.T) {
	out := Cap(&mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "Here they are."}},
		StructuredContent: map[string]any{"items": bigItems(100)},
	}, 8_000)
	assert.Equal(t, "Here they are.", resultText(t, out))
	assert.NotEmpty(t, asList(t, structuredObject(t, out)["items"]), "the list is cut to leading items")
}

// A cut text block keeps its metadata and annotations.
func TestCap_KeepsTextMetadata(t *testing.T) {
	meta := mcp.Meta{"k": "v"}
	ann := &mcp.Annotations{Audience: []mcp.Role{"user"}}
	for name, text := range map[string]string{"json": mustMarshal(t, bigItems(100)), "plain": strings.Repeat("a", 50_000)} {
		t.Run(name, func(t *testing.T) {
			out := Cap(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text, Meta: meta, Annotations: ann}}}, 8_000)
			tc, ok := out.Content[0].(*mcp.TextContent)
			require.True(t, ok)
			assert.Less(t, len(tc.Text), len(text))
			assert.Equal(t, meta, tc.Meta)
			assert.Same(t, ann, tc.Annotations)
		})
	}
}

// When not even the marker naming an oversized first item fits, the cut
// carries no continuation at all, rather than one back to the same offset.
func TestCap_UnmarkableOversizedItemGetsNoContinuation(t *testing.T) {
	items := []map[string]any{
		{"id": strings.Repeat("i", 3_000), "body": strings.Repeat("x", 20_000)},
		{"id": "2", "body": "small"},
	}
	out := Cap(JSON(map[string]any{"Drafts": items, "page": 1, "offset": 10, "filler": strings.Repeat("f", 900)}), MinBytes)
	text := resultText(t, out)
	assert.LessOrEqual(t, totalBytes(t, out), MinBytes)
	got := decode(t, text)
	if tr, ok := got["truncation"].(map[string]any); ok {
		if cont, ok := tr["continue"].(map[string]any); ok {
			assert.NotEqualValues(t, 10, cont["offset"], "a continuation back to the same offset repeats forever")
		}
	}
}

// An offset listing with two lists gets no synthesized continuation: the
// offset could belong to either.
func TestCap_AmbiguousOffsetListGetsNoContinuation(t *testing.T) {
	out := Cap(JSON(map[string]any{"items": bigItems(2), "facets": bigItems(80), "offset": 0}), 8_000)
	text := resultText(t, out)
	assert.LessOrEqual(t, totalBytes(t, out), 8_000)
	assert.NotContains(t, text, `"continue"`)
	assert.Equal(t, true, decode(t, text)["truncated"])
}
