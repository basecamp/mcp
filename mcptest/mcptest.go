// Package mcptest connects real MCP clients to servers over in-memory
// transports, so tests assert against the wire surface — tool listings,
// call results, isError — rather than internal state.
//
// Extracted from basecamp-mcp-server and hey-mcp-server, where the harness
// existed by duplication.
package mcptest

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// Connect connects a real MCP client to server over in-memory transports and
// returns the client session. Both sessions close via t.Cleanup.
func Connect(t testing.TB, server *mcp.Server) *mcp.ClientSession {
	t.Helper()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

// ListTools lists all of the session's tools keyed by name, following
// pagination cursors until the listing is complete.
func ListTools(t testing.TB, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	tools := map[string]*mcp.Tool{}
	for tool, err := range session.Tools(context.Background(), nil) {
		require.NoError(t, err)
		tools[tool.Name] = tool
	}
	return tools
}

// CallText calls tool with args and returns the first text content along with
// the result's isError flag. The call itself must succeed at the protocol
// level: in-band failures come back as isError per MCP convention.
func CallText(t testing.TB, session *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "first content must be text, got %T", res.Content[0])
	return text.Text, res.IsError
}
