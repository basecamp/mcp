package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/basecamp/mcp/gateway"
)

// This file provides a self-contained gateway server the eval can run against
// with no product repo, backend, or credentials — the fixture behind the unit
// tests and the CI smoke run. It renders the same describe surface the real
// product servers do, so SpecFromSession reads it identically.

type fakeParam struct {
	Name     string         `json:"name"`
	In       string         `json:"in"`
	Required bool           `json:"required,omitempty"`
	Schema   map[string]any `json:"schema"`
}

type fakeOp struct {
	Action      string         `json:"action"`
	Summary     string         `json:"summary"`
	ReadOnly    bool           `json:"readonly"`
	Idempotent  bool           `json:"idempotent"`
	Destructive bool           `json:"destructive"`
	Paginated   bool           `json:"paginated,omitempty"`
	Params      []fakeParam    `json:"params,omitempty"`
	Body        map[string]any `json:"body,omitempty"`
}

type fakeDomain struct {
	key, tool, title, blurb string
	ops                     []*fakeOp
}

var _ gateway.Domain = (*fakeDomain)(nil)

func (d *fakeDomain) Name() string      { return d.key }
func (d *fakeDomain) ToolName() string  { return d.tool }
func (d *fakeDomain) ToolTitle() string { return d.title }

func (d *fakeDomain) op(action string) (*fakeOp, bool) {
	for _, o := range d.ops {
		if o.Action == action {
			return o, true
		}
	}
	return nil, false
}

func (d *fakeDomain) Find(action string) (gateway.Operation, bool) {
	o, ok := d.op(action)
	if !ok {
		return gateway.Operation{}, false
	}
	return gateway.Operation{Action: o.Action, ReadOnly: o.ReadOnly}, true
}

func (d *fakeDomain) FilterReadOnly() (gateway.Domain, bool) {
	f := &fakeDomain{key: d.key, tool: d.tool, title: d.title, blurb: d.blurb}
	for _, o := range d.ops {
		if o.ReadOnly {
			f.ops = append(f.ops, o)
		}
	}
	if len(f.ops) == 0 {
		return nil, false
	}
	return f, true
}

func (d *fakeDomain) AllReadOnly() bool {
	for _, o := range d.ops {
		if !o.ReadOnly {
			return false
		}
	}
	return true
}

func (d *fakeDomain) AllIdempotent() bool {
	for _, o := range d.ops {
		if !o.ReadOnly && !o.Idempotent {
			return false
		}
	}
	return true
}

func (d *fakeDomain) AnyDestructive() bool {
	for _, o := range d.ops {
		if o.Destructive {
			return true
		}
	}
	return false
}

func (d *fakeDomain) ActionNames() []string {
	names := make([]string, 0, len(d.ops))
	for _, o := range d.ops {
		names = append(names, o.Action)
	}
	sort.Strings(names)
	return names
}

func (d *fakeDomain) Description() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nGateway tool.\n", d.blurb)
	return b.String()
}

func (d *fakeDomain) InputSchema() map[string]any {
	actions := make([]any, 0, len(d.ops)+1)
	for _, o := range d.ops {
		actions = append(actions, o.Action)
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

func (d *fakeDomain) Describe(action string) (any, error) {
	if action == "" {
		summaries := make([]map[string]any, 0, len(d.ops))
		for _, o := range d.ops {
			summaries = append(summaries, map[string]any{
				"action": o.Action, "summary": o.Summary,
				"readonly": o.ReadOnly, "destructive": o.Destructive,
			})
		}
		return map[string]any{"domain": d.key, "actions": summaries}, nil
	}
	o, ok := d.op(action)
	if !ok {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	return o, nil
}

func fakeStr(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func fakeInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// fakeDomains is a compact 4-class catalog: reads, an idempotent update, a
// non-idempotent create, a destructive delete, an idempotent publish.
func fakeDomains() []gateway.Domain {
	boards := &fakeDomain{
		key: "boards", tool: "fake_boards", title: "Fake Boards", blurb: "Boards.",
		ops: []*fakeOp{
			{Action: "create_board", Summary: "Create a board",
				Body: map[string]any{"type": "object", "required": []any{"name"},
					"properties": map[string]any{"name": fakeStr("Board name")}}},
			{Action: "delete_board", Summary: "Delete a board", Destructive: true,
				Params: []fakeParam{{Name: "board_id", In: "path", Required: true, Schema: fakeInt("Board ID")}}},
			{Action: "get_board", Summary: "Get one board", ReadOnly: true, Idempotent: true,
				Params: []fakeParam{{Name: "board_id", In: "path", Required: true, Schema: fakeInt("Board ID")}}},
			{Action: "list_boards", Summary: "List boards", ReadOnly: true, Idempotent: true, Paginated: true},
			{Action: "publish_board", Summary: "Publish a board", Idempotent: true,
				Params: []fakeParam{{Name: "board_id", In: "path", Required: true, Schema: fakeInt("Board ID")}}},
			{Action: "update_board", Summary: "Update a board", Idempotent: true,
				Params: []fakeParam{{Name: "board_id", In: "path", Required: true, Schema: fakeInt("Board ID")}},
				Body: map[string]any{"type": "object",
					"properties": map[string]any{"name": fakeStr("Board name")}}},
		},
	}
	cards := &fakeDomain{
		key: "cards", tool: "fake_cards", title: "Fake Cards", blurb: "Cards.",
		ops: []*fakeOp{
			{Action: "create_card", Summary: "Create a card",
				Params: []fakeParam{{Name: "board_id", In: "path", Required: true, Schema: fakeInt("Board ID")}},
				Body: map[string]any{"type": "object", "required": []any{"title"},
					"properties": map[string]any{
						"title":  fakeStr("Card title"),
						"status": map[string]any{"type": "string", "description": "Status", "enum": []any{"published", "drafted"}},
					}}},
			{Action: "delete_card", Summary: "Delete a card", Destructive: true,
				Params: []fakeParam{{Name: "card_number", In: "path", Required: true, Schema: fakeInt("Card number")}}},
			{Action: "get_card", Summary: "Get one card", ReadOnly: true, Idempotent: true,
				Params: []fakeParam{{Name: "card_number", In: "path", Required: true, Schema: fakeInt("Card number")}}},
			{Action: "list_cards", Summary: "List cards on a board", ReadOnly: true, Idempotent: true, Paginated: true,
				Params: []fakeParam{{Name: "board_id", In: "path", Required: true, Schema: fakeInt("Board ID")}}},
		},
	}
	return []gateway.Domain{boards, cards}
}

// NewFakeServer builds an in-process gateway MCP server over the fake catalog.
// Its handler never runs during an eval, which only lists and describes.
func NewFakeServer() (*mcp.Server, error) {
	gw, err := gateway.New(fakeDomains(), gateway.Config{
		Handler: func(context.Context, gateway.Domain, gateway.Operation, map[string]any) (*mcp.CallToolResult, error) {
			return gateway.JSONResult(map[string]any{"ok": true})
		},
	})
	if err != nil {
		return nil, err
	}
	return gw.BuildMCPServer(&mcp.Implementation{Name: "fake-mcp-server", Version: "0.0.0"}, nil), nil
}

// ConnectInProcess connects a client to server over in-memory transports,
// returning the session and a close func. Unlike mcptest.Connect it needs no
// testing.TB, so the cmd runner can drive the fake server.
func ConnectInProcess(ctx context.Context, server *mcp.Server) (*mcp.ClientSession, func(), error) {
	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "eval-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		_ = serverSession.Close()
		return nil, nil, err
	}
	return clientSession, func() { _ = clientSession.Close(); _ = serverSession.Close() }, nil
}
