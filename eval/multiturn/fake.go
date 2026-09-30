package multiturn

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is the multi-turn mode's CI fixture: a tiny projects-and-todos
// product server that does what a real product server does — gateway tools
// dispatching {action, params} to REST calls against an API base URL — so it
// runs against the cassette Player exactly as basecamp-mcp does, in process,
// with no product repo. Its guidance is switched by server flags, like a real
// server's, so the arms are exercised end to end: --instructions sends
// initialize instructions, --guide serves a fake_guide tool, --skill serves
// the skill as a resource.

// FakeOptions selects the fake server's guidance.
type FakeOptions struct {
	Instructions bool
	Guide        bool
	Skill        bool
}

// ParseFakeArgs parses an arm's server_args for the fake server.
func ParseFakeArgs(args []string) (FakeOptions, error) {
	var o FakeOptions
	fs := flag.NewFlagSet("fake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.Instructions, "instructions", false, "send server instructions")
	fs.BoolVar(&o.Guide, "guide", false, "serve the guide tool")
	fs.BoolVar(&o.Skill, "skill", false, "serve the skill resource")
	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("fake server args %v: %w", args, err)
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("fake server args: unexpected %v", fs.Args())
	}
	return o, nil
}

// FakeSkillURI is where the fake server serves its skill.
const FakeSkillURI = "skill://fake/SKILL.md"

const fakeInstructions = "Fake Todos: projects hold todos. Find a project's id with fake_projects list_projects before touching its todos. To remove a todo, use trash_todo (recoverable); delete_todo is permanent and only for explicit permanent deletion."

const fakeGuide = `# Fake Todos guide
- Projects: fake_projects {"action":"list_projects"} lists ids.
- Todos: fake_todos {"action":"list_todos","params":{"project_id":N}}.
- Removing a todo: trash_todo moves it to the trash (recoverable). delete_todo destroys it permanently — never use it when the user says trash, archive, clean up, or remove.`

const fakeSkill = `---
name: fake-todos
description: Working with Fake Todos through its MCP server.
---
Resolve names to ids first (list_projects, list_todos). Prefer trash_todo over delete_todo unless the user explicitly asks for permanent deletion.`

type fakeRoute struct {
	method, path string // path may hold {project_id} / {todo_id}
	readOnly     bool
	body         []string // params sent as the JSON body
}

var fakeRoutes = map[string]map[string]fakeRoute{
	"fake_projects": {
		"list_projects": {method: "GET", path: "/projects.json", readOnly: true},
		"get_project":   {method: "GET", path: "/projects/{project_id}.json", readOnly: true},
	},
	"fake_todos": {
		"list_todos":    {method: "GET", path: "/projects/{project_id}/todos.json", readOnly: true},
		"get_todo":      {method: "GET", path: "/todos/{todo_id}.json", readOnly: true},
		"create_todo":   {method: "POST", path: "/projects/{project_id}/todos.json", body: []string{"title"}},
		"complete_todo": {method: "POST", path: "/todos/{todo_id}/completion.json"},
		"trash_todo":    {method: "PUT", path: "/todos/{todo_id}/trash.json"},
		"delete_todo":   {method: "DELETE", path: "/todos/{todo_id}.json"},
	},
}

var fakeParamDocs = map[string]string{
	"project_id": "Project ID",
	"todo_id":    "Todo ID",
	"title":      "Todo title",
}

// NewFakeServer builds the fake product server against an API base URL.
func NewFakeServer(baseURL string, o FakeOptions) *mcp.Server {
	opts := &mcp.ServerOptions{}
	if o.Instructions {
		opts.Instructions = fakeInstructions
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-todos", Version: "0.0.0"}, opts)
	client := &http.Client{Timeout: 10 * time.Second}

	for _, tool := range []string{"fake_projects", "fake_todos"} {
		routes := fakeRoutes[tool]
		var actions []any
		var doc strings.Builder
		fmt.Fprintf(&doc, "Gateway tool: call with {\"action\": \"...\", \"params\": {...}}.\nActions:\n")
		for _, name := range sortedKeys(routes) {
			r := routes[name]
			actions = append(actions, name)
			var ps []string
			for _, p := range routeParams(r) {
				ps = append(ps, p+" ("+fakeParamDocs[p]+")")
			}
			kind := "write"
			if r.readOnly {
				kind = "read"
			}
			fmt.Fprintf(&doc, "- %s [%s] params: %s\n", name, kind, strings.Join(ps, ", "))
		}
		t := &mcp.Tool{
			Name:        tool,
			Description: doc.String(),
			InputSchema: map[string]any{
				"type":     "object",
				"required": []any{"action"},
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": actions},
					"params": map[string]any{"type": "object"},
				},
			},
		}
		srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Action string         `json:"action"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return fakeError("invalid arguments: %v", err), nil
			}
			r, ok := routes[args.Action]
			if !ok {
				return fakeError("unknown action %q", args.Action), nil
			}
			return fakeDispatch(ctx, client, baseURL, r, args.Params), nil
		})
	}

	if o.Guide {
		srv.AddTool(&mcp.Tool{
			Name:        "fake_guide",
			Description: "How to use the Fake Todos tools: ids, trash vs delete. Read-only.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fakeGuide}}}, nil
		})
	}
	if o.Skill {
		srv.AddResource(&mcp.Resource{URI: FakeSkillURI, Name: "fake-todos skill", MIMEType: "text/markdown"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: FakeSkillURI, MIMEType: "text/markdown", Text: fakeSkill}}}, nil
			})
	}
	return srv
}

func routeParams(r fakeRoute) []string {
	var ps []string
	for _, p := range []string{"project_id", "todo_id"} {
		if strings.Contains(r.path, "{"+p+"}") {
			ps = append(ps, p)
		}
	}
	return append(ps, r.body...)
}

func fakeDispatch(ctx context.Context, client *http.Client, base string, r fakeRoute, params map[string]any) *mcp.CallToolResult {
	path := r.path
	for _, p := range []string{"project_id", "todo_id"} {
		ph := "{" + p + "}"
		if !strings.Contains(path, ph) {
			continue
		}
		v, ok := params[p]
		if !ok {
			return fakeError("missing required param %q", p)
		}
		path = strings.ReplaceAll(path, ph, fmt.Sprint(v))
	}
	var body io.Reader
	if len(r.body) > 0 {
		payload := map[string]any{}
		for _, p := range r.body {
			v, ok := params[p]
			if !ok {
				return fakeError("missing required param %q", p)
			}
			payload[p] = v
		}
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, strings.TrimSuffix(base, "/")+path, body)
	if err != nil {
		return fakeError("%v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fakeError("request failed: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return fakeError("not found")
	}
	if resp.StatusCode >= 300 {
		return fakeError("HTTP %d", resp.StatusCode)
	}
	text := string(data)
	if text == "" {
		text = `{"ok":true}`
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func fakeError(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ConnectFake is a Launcher for the fake server, in process.
func ConnectFake(ctx context.Context, arm Arm, baseURL string) (*mcp.ClientSession, func(), error) {
	o, err := ParseFakeArgs(arm.ServerArgs)
	if err != nil {
		return nil, nil, err
	}
	srv := NewFakeServer(baseURL, o)
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "eval-multiturn", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		_ = ss.Close()
		return nil, nil, err
	}
	return cs, func() { _ = cs.Close(); _ = ss.Close() }, nil
}
