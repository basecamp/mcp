package result

import (
	"context"
	"encoding/json"
	"strings"
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

// What the client receives fits the cap, including what the SDK adds after
// the middleware (resultType, the serverInfo _meta): a prefix cut packs the
// result to the limit, so an unreserved cap would land over it.
func TestMiddleware_CapHoldsForWhatTheClientReceives(t *testing.T) {
	for name, payload := range map[string]any{
		"quotes":    map[string]any{"body": strings.Repeat(`"`, 24_000)},
		"list":      bigItems(100),
		"plain big": nil,
	} {
		t.Run(name, func(t *testing.T) {
			session := serve(t, 8_000, func() *mcp.CallToolResult {
				if payload == nil {
					return Text(strings.Repeat("a", 50_000))
				}
				return JSON(payload)
			})
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "annotated"})
			require.NoError(t, err)
			wire, err := json.Marshal(res)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(wire), 8_000)
			assert.Contains(t, string(wire), "truncated")
		})
	}
}

// Mirroring copies a changed tool rather than writing through to the
// server's own definition, and serves a consistent tool as is.
func TestMirrorTitlesCopiesRatherThanWritingThrough(t *testing.T) {
	annotated := &mcp.Tool{Name: "a", Annotations: &mcp.ToolAnnotations{Title: "A"}}
	titled := &mcp.Tool{Name: "b", Title: "B"}
	both := &mcp.Tool{Name: "c", Title: "C", Annotations: &mcp.ToolAnnotations{Title: "C"}}
	differ := &mcp.Tool{Name: "d", Title: "D", Annotations: &mcp.ToolAnnotations{Title: "Stale", ReadOnlyHint: true}}
	in := &mcp.ListToolsResult{Tools: []*mcp.Tool{annotated, titled, both, differ}}

	out := mirrorTitles(in)

	assert.Equal(t, "A", out.Tools[0].Title)
	assert.Equal(t, "B", out.Tools[1].Annotations.Title)
	assert.Same(t, both, out.Tools[2], "a tool already consistent is served as is")
	assert.Equal(t, "D", out.Tools[3].Title)
	assert.Equal(t, "D", out.Tools[3].Annotations.Title, "the top-level title wins when both are set and differ")
	assert.True(t, out.Tools[3].Annotations.ReadOnlyHint, "the other annotations are kept")
	assert.Equal(t, "Stale", differ.Annotations.Title, "the server's own definition is not mutated")
	assert.Empty(t, annotated.Title, "the server's own definition is not mutated")
	assert.Nil(t, titled.Annotations, "the server's own definition is not mutated")
	assert.Same(t, annotated, in.Tools[0])
}
