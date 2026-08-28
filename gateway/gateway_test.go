package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/gateway"
	"github.com/basecamp/mcp/mcptest"
)

// fakeDomain is a minimal catalog: what a product's generated or hand-curated
// catalog supplies to the gateway.
type fakeDomain struct {
	name        string
	title       string
	ops         []gateway.Operation
	destructive bool // reported by AnyDestructive
	idempotent  bool // reported by AllIdempotent for domains with writes
}

func (d *fakeDomain) Name() string      { return d.name }
func (d *fakeDomain) ToolName() string  { return "test_" + d.name }
func (d *fakeDomain) ToolTitle() string { return d.title }
func (d *fakeDomain) Description() string {
	return "Test domain " + d.name + "\n\nACTIONS: " + fmt.Sprint(d.ActionNames())
}

func (d *fakeDomain) InputSchema() map[string]any {
	actions := make([]any, 0, len(d.ops)+1)
	for _, op := range d.ops {
		actions = append(actions, op.Action)
	}
	actions = append(actions, gateway.DescribeAction)
	return map[string]any{
		"type":                 "object",
		"required":             []any{"action"},
		"additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": actions},
			"params": map[string]any{"type": "object"},
		},
	}
}

func (d *fakeDomain) AllReadOnly() bool {
	for _, op := range d.ops {
		if !op.ReadOnly {
			return false
		}
	}
	return true
}

func (d *fakeDomain) AllIdempotent() bool {
	return d.AllReadOnly() || d.idempotent
}

func (d *fakeDomain) AnyDestructive() bool { return d.destructive }

func (d *fakeDomain) ActionNames() []string {
	names := make([]string, 0, len(d.ops))
	for _, op := range d.ops {
		names = append(names, op.Action)
	}
	sort.Strings(names)
	return names
}

func (d *fakeDomain) Find(action string) (gateway.Operation, bool) {
	for _, op := range d.ops {
		if op.Action == action {
			return op, true
		}
	}
	return gateway.Operation{}, false
}

func (d *fakeDomain) Describe(action string) (any, error) {
	if action == "" {
		return map[string]any{"domain": d.name, "actions": d.ActionNames()}, nil
	}
	op, ok := d.Find(action)
	if !ok {
		return nil, fmt.Errorf("unknown action %q in domain %q", action, d.name)
	}
	return map[string]any{"action": op.Action, "readonly": op.ReadOnly}, nil
}

func (d *fakeDomain) FilterReadOnly() (gateway.Domain, bool) {
	filtered := &fakeDomain{name: d.name, title: d.title}
	for _, op := range d.ops {
		if op.ReadOnly {
			filtered.ops = append(filtered.ops, op)
		}
	}
	if len(filtered.ops) == 0 {
		return nil, false
	}
	return filtered, true
}

func testDomains() []gateway.Domain {
	return []gateway.Domain{
		&fakeDomain{name: "boxes", title: "Test Boxes", destructive: true, ops: []gateway.Operation{
			{Action: "get_imbox", ReadOnly: true},
			{Action: "list_boxes", ReadOnly: true},
			{Action: "create_box_group", ReadOnly: false},
		}},
		&fakeDomain{name: "search", title: "Test Search", ops: []gateway.Operation{
			{Action: "search", ReadOnly: true},
		}},
		&fakeDomain{name: "todos", title: "Test Todos", idempotent: true, ops: []gateway.Operation{
			{Action: "create_todo", ReadOnly: false},
		}},
	}
}

// echoHandler reports what was dispatched, standing in for a product handler.
func echoHandler(ctx context.Context, d gateway.Domain, op gateway.Operation, params map[string]any) (*mcp.CallToolResult, error) {
	return gateway.JSONResult(map[string]any{
		"domain": d.Name(),
		"action": op.Action,
		"params": params,
	})
}

func connect(t *testing.T, cfg gateway.Config) *mcp.ClientSession {
	t.Helper()
	if cfg.Handler == nil {
		cfg.Handler = echoHandler
	}
	srv, err := gateway.New(testDomains(), cfg)
	require.NoError(t, err)
	impl := &mcp.Implementation{Name: "gateway-test", Version: "0.0.0"}
	return mcptest.Connect(t, srv.BuildMCPServer(impl, slog.New(slog.DiscardHandler)))
}

func TestListToolsServesOneToolPerDomain(t *testing.T) {
	session := connect(t, gateway.Config{})
	tools := mcptest.ListTools(t, session)

	require.Len(t, tools, 3)
	for _, name := range []string{"test_boxes", "test_search", "test_todos"} {
		require.Contains(t, tools, name)
		require.NotNil(t, tools[name].Annotations, "tool %q", name)
	}

	// search is all read-only; todos is not.
	assert.True(t, tools["test_search"].Annotations.ReadOnlyHint)
	assert.False(t, tools["test_todos"].Annotations.ReadOnlyHint)

	// The domain's input schema constrains action to its enum.
	schema, ok := tools["test_boxes"].InputSchema.(map[string]any)
	require.True(t, ok)
	enum := schema["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]any)
	assert.Contains(t, enum, "get_imbox")
	assert.Contains(t, enum, "describe")
}

func TestDispatchRunsHandlerWithParams(t *testing.T) {
	session := connect(t, gateway.Config{})

	text, isError := mcptest.CallText(t, session, "test_boxes", map[string]any{
		"action": "get_imbox",
		"params": map[string]any{"page": float64(2)},
	})
	require.False(t, isError, "dispatch failed: %s", text)
	assert.Contains(t, text, `"domain": "boxes"`)
	assert.Contains(t, text, `"action": "get_imbox"`)
	assert.Contains(t, text, `"page": 2`)
}

func TestDescribeServesDomainAndAction(t *testing.T) {
	session := connect(t, gateway.Config{})

	text, isError := mcptest.CallText(t, session, "test_boxes", map[string]any{"action": "describe"})
	require.False(t, isError, "describe failed: %s", text)
	assert.Contains(t, text, `"domain": "boxes"`)

	text, isError = mcptest.CallText(t, session, "test_boxes", map[string]any{
		"action": "describe",
		"params": map[string]any{"action": "get_imbox"},
	})
	require.False(t, isError, "describe failed: %s", text)
	assert.Contains(t, text, `"action": "get_imbox"`)
}

func TestDescribeRejectsNonStringTarget(t *testing.T) {
	session := connect(t, gateway.Config{})

	text, isError := mcptest.CallText(t, session, "test_boxes", map[string]any{
		"action": "describe",
		"params": map[string]any{"action": 123},
	})
	assert.True(t, isError)
	assert.Contains(t, text, "params.action must be a string")
}

func TestUnknownActionIsInBandError(t *testing.T) {
	session := connect(t, gateway.Config{})

	text, isError := mcptest.CallText(t, session, "test_boxes", map[string]any{"action": "no_such_action"})
	assert.True(t, isError)
	assert.Contains(t, text, "unknown action")
	assert.Contains(t, text, "list_boxes")
	assert.Contains(t, text, "describe", "the reserved action is part of the advertised surface")
}

func TestReadOnlyModeFiltersWriteActions(t *testing.T) {
	session := connect(t, gateway.Config{ReadOnly: true})
	tools := mcptest.ListTools(t, session)

	// todos is all writes, so the whole tool drops.
	assert.NotContains(t, tools, "test_todos")

	for name, tool := range tools {
		require.NotNil(t, tool.Annotations, "tool %q", name)
		assert.True(t, tool.Annotations.ReadOnlyHint, "tool %q must be read-only", name)
		schema := tool.InputSchema.(map[string]any)
		enum := schema["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]any)
		assert.NotContains(t, enum, "create_box_group", "tool %q", name)
	}

	// A filtered write action is gone from the catalog, so dispatch refuses
	// it in-band even when a client ignores the schema.
	text, isError := mcptest.CallText(t, session, "test_boxes", map[string]any{"action": "create_box_group"})
	assert.True(t, isError)
	assert.Contains(t, text, "unknown action", "filtered actions are not in the read-only catalog: %s", text)
}

func TestReadOnlyRefusesWriteDispatch(t *testing.T) {
	// A domain that reports a write operation from Find even in read-only
	// mode (a catalog whose FilterReadOnly is incomplete) still cannot get a
	// write dispatched: the gate is checked at dispatch time too.
	leaky := &fakeDomain{name: "leaky", title: "Test Leaky", ops: []gateway.Operation{
		{Action: "read_thing", ReadOnly: true},
		{Action: "write_thing", ReadOnly: false},
	}}
	srv, err := gateway.New([]gateway.Domain{leakyNoFilter{leaky}}, gateway.Config{ReadOnly: true, Handler: echoHandler})
	require.NoError(t, err)
	impl := &mcp.Implementation{Name: "gateway-test", Version: "0.0.0"}
	session := mcptest.Connect(t, srv.BuildMCPServer(impl, slog.New(slog.DiscardHandler)))

	text, isError := mcptest.CallText(t, session, "test_leaky", map[string]any{"action": "write_thing"})
	assert.True(t, isError)
	assert.Contains(t, text, "not available in read-only mode")
}

// leakyNoFilter passes itself through FilterReadOnly unchanged, simulating a
// catalog that forgets to drop write operations.
type leakyNoFilter struct{ *fakeDomain }

func (d leakyNoFilter) FilterReadOnly() (gateway.Domain, bool) { return d, true }

func TestUnknownDomainFailsClosed(t *testing.T) {
	_, err := gateway.New(testDomains(), gateway.Config{Domains: []string{"boxes", "nope"}, Handler: echoHandler})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown domain "nope"`)
	assert.Contains(t, err.Error(), "boxes, search, todos")
}

func TestDomainNarrowing(t *testing.T) {
	session := connect(t, gateway.Config{Domains: []string{"search"}})
	tools := mcptest.ListTools(t, session)
	assert.Len(t, tools, 1)
	assert.Contains(t, tools, "test_search")
}

func TestReservedDescribeActionFailsClosed(t *testing.T) {
	// An operation registered under the reserved describe action would be
	// silently unreachable: dispatch always routes "describe" to
	// Domain.Describe. Refuse the catalog at startup instead.
	bad := &fakeDomain{name: "bad", title: "Test Bad", ops: []gateway.Operation{
		{Action: "describe", ReadOnly: true},
	}}
	_, err := gateway.New([]gateway.Domain{bad}, gateway.Config{Handler: echoHandler})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `reserved`)
	assert.Contains(t, err.Error(), `"bad"`)
}

func TestMissingHandlerIsAStartupError(t *testing.T) {
	_, err := gateway.New(testDomains(), gateway.Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Handler is required")
}

func TestNewRejectsUntitledDomains(t *testing.T) {
	untitled := &fakeDomain{name: "untitled", ops: []gateway.Operation{
		{Action: "read_thing", ReadOnly: true},
	}}
	_, err := gateway.New([]gateway.Domain{untitled}, gateway.Config{Handler: echoHandler})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `domain "untitled" has no tool title`)
}

// TestAnnotationsDeriveFromDomain proves every safety hint is derived and
// explicit: DestructiveHint is tri-state with an absent-means-true default,
// so clean tools must say false out loud; OpenWorldHint is always false for
// a bounded product API; IdempotentHint comes from the catalog, never a
// hardcoded value.
func TestAnnotationsDeriveFromDomain(t *testing.T) {
	session := connect(t, gateway.Config{})
	tools := mcptest.ListTools(t, session)

	boxes := tools["test_boxes"].Annotations
	assert.Equal(t, "Test Boxes", boxes.Title)
	assert.False(t, boxes.ReadOnlyHint)
	require.NotNil(t, boxes.DestructiveHint)
	assert.True(t, *boxes.DestructiveHint, "boxes has a destructive write")
	assert.False(t, boxes.IdempotentHint)

	search := tools["test_search"].Annotations
	assert.Equal(t, "Test Search", search.Title)
	assert.True(t, search.ReadOnlyHint)
	require.NotNil(t, search.DestructiveHint)
	assert.False(t, *search.DestructiveHint, "read-only tools are never destructive")
	assert.True(t, search.IdempotentHint, "all-read domains are idempotent")

	todos := tools["test_todos"].Annotations
	require.NotNil(t, todos.DestructiveHint)
	assert.False(t, *todos.DestructiveHint, "writes without destructive operations say so explicitly")
	assert.True(t, todos.IdempotentHint)

	for name, tool := range tools {
		require.NotNil(t, tool.Annotations.OpenWorldHint, "tool %q", name)
		assert.False(t, *tool.Annotations.OpenWorldHint, "tool %q: a product API is a closed world", name)
		assert.NotEmpty(t, tool.Annotations.Title, "tool %q", name)
		if tool.Annotations.ReadOnlyHint {
			assert.False(t, *tool.Annotations.DestructiveHint,
				"tool %q: readOnlyHint and destructiveHint must never both be true", name)
		}
	}
}

// TestReadOnlyModeAnnotationsFollowTheFilter proves annotations are computed
// after read-only filtering: a domain that is destructive when its writes are
// served becomes a clean read-only tool when they are filtered out.
func TestReadOnlyModeAnnotationsFollowTheFilter(t *testing.T) {
	session := connect(t, gateway.Config{ReadOnly: true})
	tools := mcptest.ListTools(t, session)

	boxes := tools["test_boxes"].Annotations
	assert.True(t, boxes.ReadOnlyHint)
	require.NotNil(t, boxes.DestructiveHint)
	assert.False(t, *boxes.DestructiveHint, "filtered surface has no destructive writes left")
	assert.True(t, boxes.IdempotentHint)
}

// TestOverviewAggregatesServedDomains proves the whole-surface payload is
// honest by construction: it reflects the served catalog after read-only
// filtering and narrowing, so a filtered action or domain never appears.
func TestOverviewAggregatesServedDomains(t *testing.T) {
	srv, err := gateway.New(testDomains(), gateway.Config{Handler: echoHandler})
	require.NoError(t, err)
	payload, err := srv.Overview()
	require.NoError(t, err)
	rendered, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.Contains(t, string(rendered), `"test_boxes"`)
	assert.Contains(t, string(rendered), `"Test Boxes"`)
	assert.Contains(t, string(rendered), `"test_todos"`)
	assert.Contains(t, string(rendered), "create_todo")

	narrowed, err := gateway.New(testDomains(), gateway.Config{ReadOnly: true, Handler: echoHandler})
	require.NoError(t, err)
	payload, err = narrowed.Overview()
	require.NoError(t, err)
	rendered, err = json.Marshal(payload)
	require.NoError(t, err)
	assert.NotContains(t, string(rendered), `"test_todos"`, "all-write domains drop from the read-only overview")
	assert.NotContains(t, string(rendered), "create_box_group", "filtered writes drop from the overview")
}
