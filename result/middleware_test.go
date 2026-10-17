package result

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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

// What the client receives fits the cap: the whole JSON-RPC response frame,
// as the server writes it, including what the SDK adds after the middleware
// has measured the result (resultType, the serverInfo _meta, the frame
// around the result). A prefix cut packs the result to the limit less
// frameReserve, so the reserve is all that keeps these under it. The result
// the client decodes is not the measure: it drops resultType and the frame.
func TestMiddleware_CapHoldsForTheResponseFrame(t *testing.T) {
	// Identified the way a product server is, by name and a module
	// pseudo-version.
	impl := &mcp.Implementation{Name: "example-mcp-server", Version: "v0.0.0-20261017204500-0123456789ab"}
	payloads := map[string]func() *mcp.CallToolResult{
		"quotes":     func() *mcp.CallToolResult { return JSON(map[string]any{"body": strings.Repeat(`"`, 24_000)}) },
		"html":       func() *mcp.CallToolResult { return JSON(map[string]any{"body": strings.Repeat(`<&>`, 24_000)}) },
		"long field": func() *mcp.CallToolResult { return JSON(map[string]any{"body": strings.Repeat("x", 60_000)}) },
		"list":       func() *mcp.CallToolResult { return JSON(bigItems(100)) },
		"plain big":  func() *mcp.CallToolResult { return Text(strings.Repeat("a", 50_000)) },
	}
	for _, limit := range []int{MinBytes, 8_000} {
		for name, answer := range payloads {
			t.Run(fmt.Sprintf("%s/%d", name, limit), func(t *testing.T) {
				frame := callFrame(t, impl, limit, answer)
				assert.LessOrEqual(t, len(frame), limit)
				assert.Contains(t, frame, "truncated")
				assert.Contains(t, frame, `"resultType"`, "the frame carries what the SDK adds after the middleware")
				assert.Contains(t, frame, impl.Version, "the frame carries the serverInfo _meta")
			})
		}
	}
}

// callFrame serves one tool behind Middleware(maxBytes), calls it, and
// returns the tools/call response as the server wrote it to its transport.
func callFrame(t *testing.T, impl *mcp.Implementation, maxBytes int, answer func() *mcp.CallToolResult) string {
	t.Helper()
	server := mcp.NewServer(impl, nil)
	server.AddTool(&mcp.Tool{Name: "t", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return answer(), nil })
	server.AddReceivingMiddleware(Middleware(maxBytes))

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	recorder := &recordingTransport{Transport: serverTransport}
	serverSession, err := server.Connect(context.Background(), recorder, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSession.Close() })

	_, err = clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "t"})
	require.NoError(t, err)

	frame := recorder.last(`"content"`)
	require.NotEmpty(t, frame, "no tools/call response was written")
	return frame
}

// recordingTransport keeps every message the server writes, encoded the way
// the stdio and HTTP transports encode it, before handing it on: the client
// can't have the response before it is recorded.
type recordingTransport struct {
	mcp.Transport
	mu      sync.Mutex
	written []string
}

func (r *recordingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := r.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingConn{Connection: conn, recorder: r}, nil
}

// last returns the last written message containing marker.
func (r *recordingTransport) last(marker string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.written) - 1; i >= 0; i-- {
		if strings.Contains(r.written[i], marker) {
			return r.written[i]
		}
	}
	return ""
}

type recordingConn struct {
	mcp.Connection
	recorder *recordingTransport
}

func (c *recordingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	c.recorder.mu.Lock()
	c.recorder.written = append(c.recorder.written, string(data))
	c.recorder.mu.Unlock()
	return c.Connection.Write(ctx, msg)
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
