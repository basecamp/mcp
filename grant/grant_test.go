package grant_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/grant"
	"github.com/basecamp/mcp/mcptest"
)

const prm = "https://mcp.example.com/.well-known/oauth-protected-resource"

// fixture is a server with a read tool, a write tool, a send tool and an
// ungoverned tool, whose grant each test sets. ran counts handler runs.
type fixture struct {
	scope  atomic.Value // string
	scoped atomic.Bool
	ran    atomic.Int32
}

func (f *fixture) policy() grant.Policy {
	needs := map[string][]string{
		"get_thing":  {"read"},
		"move_thing": {"write"},
		"send_thing": {"send", "read"},
	}
	return grant.Policy{
		Needs: func(tool string) ([]string, bool) { n, ok := needs[tool]; return n, ok },
		Grant: func(context.Context, mcp.Request) (string, bool) {
			return f.scope.Load().(string), f.scoped.Load()
		},
		Implies: map[string][]string{"full": {"read", "write", "send"}, "write": {"read"}},
		Refuse: func(_ context.Context, s grant.Shortfall) *mcp.CallToolResult {
			return grant.Refusal(
				fmt.Sprintf("%s needs the %s permission. Reconnect to allow it.", s.Tool, strings.Join(s.Missing, " and ")),
				grant.Challenge(prm, s.Ask, "Reconnect to allow "+strings.Join(s.Missing, " and ")))
		},
	}
}

func (f *fixture) session(t *testing.T, scope string, scoped bool) *mcp.ClientSession {
	t.Helper()
	f.scope.Store(scope)
	f.scoped.Store(scoped)
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0"}, nil)
	for _, name := range []string{"get_thing", "move_thing", "send_thing", "ping"} {
		mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			f.ran.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran"}}}, nil, nil
		})
	}
	server.AddReceivingMiddleware(f.policy().Middleware())
	return mcptest.Connect(t, server)
}

func names(t *testing.T, s *mcp.ClientSession) []string {
	var out []string
	for name := range mcptest.ListTools(t, s) {
		out = append(out, name)
	}
	return out
}

func TestListingFollowsTheGrant(t *testing.T) {
	cases := []struct {
		scope  string
		scoped bool
		want   []string
	}{
		{"read", true, []string{"get_thing", "ping"}},
		{"read write", true, []string{"get_thing", "move_thing", "ping"}},
		{"write", true, []string{"get_thing", "move_thing", "ping"}},
		{"read write send", true, []string{"get_thing", "move_thing", "send_thing", "ping"}},
		{"full", true, []string{"get_thing", "move_thing", "send_thing", "ping"}},
		// send implies nothing, so a send-only grant can't run send_thing.
		{"send", true, []string{"ping"}},
		{"", true, []string{"ping"}},
		{"", false, []string{"get_thing", "move_thing", "send_thing", "ping"}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q scoped=%v", c.scope, c.scoped), func(t *testing.T) {
			var f fixture
			assert.ElementsMatch(t, c.want, names(t, f.session(t, c.scope, c.scoped)))
		})
	}
}

func TestACallTheGrantCantRunIsRefusedInBand(t *testing.T) {
	var f fixture
	s := f.session(t, "read write", true)

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_thing"})
	require.NoError(t, err, "a refusal is a tool result, never a protocol error")
	assert.True(t, res.IsError)
	assert.Equal(t, int32(0), f.ran.Load(), "the handler must not run")
	require.Len(t, res.Content, 1)
	assert.Equal(t, "send_thing needs the send permission. Reconnect to allow it.", res.Content[0].(*mcp.TextContent).Text)

	challenges, ok := res.Meta[grant.MetaWWWAuthenticate].([]any)
	require.True(t, ok, "the challenge is an array on the wire, got %T", res.Meta[grant.MetaWWWAuthenticate])
	require.Len(t, challenges, 1)
	assert.Equal(t,
		`Bearer error="insufficient_scope", scope="read write send", resource_metadata="`+prm+`", error_description="Reconnect to allow send"`,
		challenges[0])
}

func TestACallTheGrantCanRunRuns(t *testing.T) {
	var f fixture
	s := f.session(t, "write", true)
	text, isErr := mcptest.CallText(t, s, "move_thing", nil)
	assert.False(t, isErr)
	assert.Equal(t, "ran", text)

	text, isErr = mcptest.CallText(t, s, "ping", nil)
	assert.False(t, isErr, "an ungoverned tool runs on any grant")
	assert.Equal(t, "ran", text)
	assert.Equal(t, int32(2), f.ran.Load())
}

func TestAnUnscopedRequestRunsEverything(t *testing.T) {
	var f fixture
	s := f.session(t, "", false)
	_, isErr := mcptest.CallText(t, s, "send_thing", nil)
	assert.False(t, isErr)
	assert.Equal(t, int32(1), f.ran.Load())
}

func TestShortfallAsksForGrantedThenMissing(t *testing.T) {
	var f fixture
	p := f.policy()
	f.scope.Store("send")
	f.scoped.Store(true)
	s, short := p.Shortfall(context.Background(), nil, "send_thing")
	require.True(t, short)
	assert.Equal(t, []string{"send"}, s.Granted)
	assert.Equal(t, []string{"read"}, s.Missing)
	assert.Equal(t, []string{"send", "read"}, s.Ask)
}

func TestExpand(t *testing.T) {
	implies := map[string][]string{"full": {"write", "send"}, "write": {"read"}}
	assert.Equal(t, []string{"full", "write", "read", "send"}, grant.Expand(implies, []string{"full", "read"}))
	assert.Empty(t, grant.Expand(implies, nil))
}

func TestChallengeEscapesQuotedValues(t *testing.T) {
	got := grant.Challenge(`https://x/"a"`, []string{"read"}, "say \"hi\"\\\nnow")
	assert.Equal(t, `Bearer error="insufficient_scope", scope="read", resource_metadata="https://x/\"a\"", error_description="say \"hi\"\\ now"`, got)
}
