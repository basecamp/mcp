package mcptest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testServer builds a minimal MCP server with one echoing tool and one tool
// that fails in-band, mirroring how product servers use the harness.
func testServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "mcptest-fixture", Version: "0.0.0"}, nil)
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"text": map[string]any{"type": "string"}},
		"additionalProperties": false,
	}
	server.AddTool(&mcp.Tool{Name: "echo", Description: "echoes text back", InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Text string `json:"text"`
			}
			raw, err := json.Marshal(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: args.Text}},
			}, nil
		})
	server.AddTool(&mcp.Tool{Name: "fail", Description: "always fails in-band", InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "expected failure"}},
			}, nil
		})
	return server
}

func TestConnectListsToolsOverTheWire(t *testing.T) {
	session := Connect(t, testServer())

	tools := ListTools(t, session)
	require.Len(t, tools, 2)
	require.Contains(t, tools, "echo")
	require.Contains(t, tools, "fail")
	assert.Equal(t, "echoes text back", tools["echo"].Description)
}

func TestListToolsFollowsPagination(t *testing.T) {
	// A one-tool page size forces the listing across protocol pages; the
	// helper must follow cursors rather than stop at the first page.
	server := mcp.NewServer(&mcp.Implementation{Name: "mcptest-fixture", Version: "0.0.0"}, &mcp.ServerOptions{PageSize: 1})
	schema := map[string]any{"type": "object"}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		server.AddTool(&mcp.Tool{Name: name, Description: name, InputSchema: schema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}

	session := Connect(t, server)
	tools := ListTools(t, session)
	require.Len(t, tools, 3)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		assert.Contains(t, tools, name)
	}
}

func TestCallTextRoundTrips(t *testing.T) {
	session := Connect(t, testServer())

	text, isError := CallText(t, session, "echo", map[string]any{"text": "hello"})
	assert.Equal(t, "hello", text)
	assert.False(t, isError)
}

func TestCallTextSurfacesInBandErrors(t *testing.T) {
	session := Connect(t, testServer())

	text, isError := CallText(t, session, "fail", map[string]any{})
	assert.Equal(t, "expected failure", text)
	assert.True(t, isError, "in-band failures must surface as isError, not protocol errors")
}

func TestSnapshotMatchesGolden(t *testing.T) {
	path := filepath.Join("testdata", "snapshot_fixture.txt")
	Snapshot(t, path, []byte("stable output\n"))
}

func TestSnapshotUpdateRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.snap")

	prev := *update
	*update = true
	defer func() { *update = prev }()

	Snapshot(t, path, []byte("first write\n"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "first write\n", string(data))
}
