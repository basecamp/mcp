package grant_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/grant"
	"github.com/basecamp/mcp/mcptest"
)

func declaredServer(t *testing.T) (*mcp.Server, map[string]*mcp.Tool) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0"}, nil)
	tools := map[string]*mcp.Tool{
		"get_thing":  {Name: "get_thing", Meta: mcp.Meta{"openai/profile": true}},
		"move_thing": {Name: "move_thing"},
		"ping":       {Name: "ping"},
	}
	for _, tool := range tools {
		mcp.AddTool(server, tool, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	}
	needs := map[string][]string{"get_thing": {"mcp", "read"}, "move_thing": {"mcp", "full"}}
	server.AddReceivingMiddleware(grant.Declare(func(tool string) ([]string, bool) {
		s, ok := needs[tool]
		return s, ok
	}))
	return server, tools
}

func schemesJSON(t *testing.T, tool *mcp.Tool) string {
	t.Helper()
	data, err := json.Marshal(tool.Meta[grant.MetaSecuritySchemes])
	require.NoError(t, err)
	return string(data)
}

func TestDeclareStampsEachGovernedTool(t *testing.T) {
	server, _ := declaredServer(t)
	listed := mcptest.ListTools(t, mcptest.Connect(t, server))

	assert.JSONEq(t, `[{"type":"oauth2","scopes":["mcp","read"]}]`, schemesJSON(t, listed["get_thing"]))
	assert.JSONEq(t, `[{"type":"oauth2","scopes":["mcp","full"]}]`, schemesJSON(t, listed["move_thing"]))
	assert.Equal(t, true, listed["get_thing"].Meta["openai/profile"], "a tool's other _meta keys are kept")
	assert.NotContains(t, listed["ping"].Meta, grant.MetaSecuritySchemes, "a tool scopes doesn't name is left as it is")
}

func TestDeclareCopiesRatherThanChanges(t *testing.T) {
	served := &mcp.ListToolsResult{Tools: []*mcp.Tool{
		{Name: "get_thing", Meta: mcp.Meta{"openai/profile": true}},
		{Name: "move_thing"},
	}}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return served, nil }
	declare := grant.Declare(func(string) ([]string, bool) { return []string{"read"}, true })

	res, err := declare(next)(context.Background(), "tools/list", &mcp.ListToolsRequest{})
	require.NoError(t, err)
	listed := res.(*mcp.ListToolsResult)
	require.Len(t, listed.Tools, 2)
	assert.Contains(t, listed.Tools[0].Meta, grant.MetaSecuritySchemes)

	assert.NotSame(t, served, listed)
	assert.Equal(t, mcp.Meta{"openai/profile": true}, served.Tools[0].Meta, "the served tool's _meta is untouched")
	assert.Nil(t, served.Tools[1].Meta)
}

func TestDeclareComposesWithThePolicy(t *testing.T) {
	var f fixture
	f.scope.Store("read")
	f.scoped.Store(true)
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0"}, nil)
	for _, name := range []string{"get_thing", "move_thing"} {
		mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	}
	p := f.policy()
	server.AddReceivingMiddleware(p.Middleware(), grant.Declare(p.Needs))

	listed := mcptest.ListTools(t, mcptest.Connect(t, server))
	require.Len(t, listed, 1, "the policy still lists only what the grant runs")
	assert.JSONEq(t, `[{"type":"oauth2","scopes":["read"]}]`, schemesJSON(t, listed["get_thing"]))
}

func TestOAuth2WritesEmptyScopes(t *testing.T) {
	data, err := json.Marshal(grant.OAuth2())
	require.NoError(t, err)
	assert.JSONEq(t, `[{"type":"oauth2","scopes":[]}]`, string(data))
}
