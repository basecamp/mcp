package result

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/mcptest"
)

// serve builds a server with the middleware installed and two tools: one
// titled only in its annotations, one only at the top level, both answering
// with whatever answer returns.
func serve(t *testing.T, maxBytes int, answer func() *mcp.CallToolResult) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	handler := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return answer(), nil
	}
	server.AddTool(&mcp.Tool{
		Name:        "annotated",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{Title: "Annotated"},
	}, handler)
	server.AddTool(&mcp.Tool{
		Name:        "titled",
		Title:       "Titled",
		InputSchema: map[string]any{"type": "object"},
	}, handler)
	server.AddReceivingMiddleware(Middleware(maxBytes))
	return mcptest.Connect(t, server)
}

// Over the wire, both titles carry the same name whichever one a tool set.
func TestMiddleware_MirrorsTitles(t *testing.T) {
	tools := mcptest.ListTools(t, serve(t, 0, func() *mcp.CallToolResult { return Text("ok") }))
	for name, want := range map[string]string{"annotated": "Annotated", "titled": "Titled"} {
		tool := tools[name]
		require.NotNil(t, tool, name)
		assert.Equal(t, want, tool.Title, name)
		require.NotNil(t, tool.Annotations, name)
		assert.Equal(t, want, tool.Annotations.Title, name)
	}
}

// A handler's bare array reaches the client as {"items": [...]}, in the
// structured content and the text alike.
func TestMiddleware_Object(t *testing.T) {
	session := serve(t, 0, func() *mcp.CallToolResult {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: `[{"id":1}]`}},
			StructuredContent: []map[string]int{{"id": 1}},
		}
	})
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "titled"})
	require.NoError(t, err)
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	assert.JSONEq(t, `{"items":[{"id":1}]}`, string(raw))
	assert.JSONEq(t, `{"items":[{"id":1}]}`, resultText(t, res))
}

// The cap holds over the wire, and a list served by JSON is cut to whole
// leading items.
func TestMiddleware_Caps(t *testing.T) {
	session := serve(t, 8_000, func() *mcp.CallToolResult { return JSON(bigItems(100)) })
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "annotated"})
	require.NoError(t, err)
	assert.LessOrEqual(t, wireLen(t, res), 8_000)
	got := decode(t, resultText(t, res))
	assert.Equal(t, true, got["truncated"])
	assert.NotEmpty(t, asList(t, got["items"]))
}
