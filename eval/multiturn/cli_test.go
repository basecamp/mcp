package multiturn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test binary doubles as a fake `claude` CLI and as the bridge, so the
// CLI backend is exercised end to end — config file, bridge process, surface
// server, Episode.Call — with no real CLI or model.
func TestMain(m *testing.M) {
	switch os.Getenv("EVAL_FAKE_ROLE") {
	case "bridge":
		if err := Bridge(os.Args[len(os.Args)-1], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "claude":
		if err := fakeClaude(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeClaude connects to the configured MCP server, checks what it was
// given, plays EVAL_FAKE_CALLS, and prints a result in the CLI's shape.
func fakeClaude() error {
	args := os.Args[1:]
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	data, err := os.ReadFile(flag("--mcp-config"))
	if err != nil {
		return err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	srv, ok := cfg.MCPServers["eval"]
	if !ok {
		return fmt.Errorf("no eval server in config")
	}
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Env = append(os.Environ(), "EVAL_FAKE_ROLE=bridge")
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "0"}, nil)
	ctx := context.Background()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	defer session.Close()

	var calls []ScriptCall
	if err := json.Unmarshal([]byte(os.Getenv("EVAL_FAKE_CALLS")), &calls); err != nil {
		return err
	}
	var seen []string
	for _, c := range calls {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: c.Tool, Arguments: c.Arguments})
		if err != nil {
			return err
		}
		seen = append(seen, fmt.Sprint(res.IsError))
	}
	instr := session.InitializeResult().Instructions
	out := map[string]any{
		"result":    fmt.Sprintf("done; instructions=%v; system-has-instructions=%v", instr != "", strings.Contains(flag("--system-prompt"), "MCP server instructions")),
		"is_error":  false,
		"subtype":   "success",
		"num_turns": len(calls) + 1,
		"modelUsage": map[string]any{flag("--model"): map[string]any{
			"inputTokens": 10, "outputTokens": 5, "cacheReadInputTokens": 100, "cacheCreationInputTokens": 20, "costUSD": 0.0123,
		}},
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return err
	}
	if os.Getenv("EVAL_FAKE_EXIT") != "" {
		os.Exit(3)
	}
	return nil
}

func TestCLIAgentEndToEnd(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("EVAL_CLAUDE_BIN", self)
	t.Setenv("EVAL_FAKE_ROLE", "claude")
	defer os.Unsetenv("EVAL_FAKE_ROLE")
	calls, _ := json.Marshal([]ScriptCall{
		{Tool: "fake_projects", Arguments: map[string]any{"action": "list_projects"}},
		{Tool: "fake_todos", Arguments: map[string]any{"action": "complete_todo", "params": map[string]any{"todo_id": 11}}},
	})
	t.Setenv("EVAL_FAKE_CALLS", string(calls))

	agent := NewCLIAgent("haiku", ModelID("haiku"), []string{self})
	r := runOne(t, "complete-todo", "instructions", agent)
	assert.True(t, r.Pass, r.Reasons)
	assert.Equal(t, 2, r.Calls)
	assert.Equal(t, 3, r.Turns)
	assert.Equal(t, 0.0123, r.CostUSD, "the CLI's own cost figure")
	assert.Equal(t, 30, r.InTokens+r.CacheWriteTokens)
	// The host receives the instructions through initialize, and the harness
	// prompt does not repeat them.
	assert.Equal(t, "done; instructions=true; system-has-instructions=false", r.Answer)
	require.Len(t, r.Trace, 2)
	assert.Equal(t, "complete_todo", r.Trace[1].Op)
	assert.Len(t, r.Trace[1].Requests, 1)
}

func TestCLIAgentHonorsANonzeroExit(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("EVAL_CLAUDE_BIN", self)
	t.Setenv("EVAL_FAKE_ROLE", "claude")
	t.Setenv("EVAL_FAKE_EXIT", "1")
	t.Setenv("EVAL_FAKE_CALLS", `[{"tool":"fake_todos","arguments":{"action":"complete_todo","params":{"todo_id":11}}}]`)
	r := runOne(t, "complete-todo", "bare", NewCLIAgent("haiku", ModelID("haiku"), []string{self}))
	assert.False(t, r.Pass)
	assert.Contains(t, r.Error, "exit status 3")
}
