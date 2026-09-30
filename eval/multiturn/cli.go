package multiturn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CLIAgent runs each episode through the local `claude` CLI in print mode —
// Claude Code as the MCP host, the way the plugin case meets the server — and
// needs no API key. The CLI is pointed at one MCP server: a bridge process it
// spawns, which relays to a surface server this process runs over the
// episode (ServeSurface). So every call still goes through Episode.Call —
// same arm filtering, same backend attribution, same grading as the API
// backend — while the host decides how server instructions reach the model
// (Claude Code puts them in its own system prompt, so the harness prompt
// leaves them out).
//
// Isolation: the CLI runs with the harness system prompt in place of its
// default, no built-in tools, only the bridge as an MCP server, no setting
// sources, and no session persistence. Token counts and cost are the CLI's
// own report for the model.
type CLIAgent struct {
	label, modelID string
	bin            string
	bridge         []string // argv prefix that, given a socket path, relays stdio to it
	timeout        time.Duration
}

// NewCLIAgent builds a CLI-backed agent. bridge is the argv (before the
// socket path) of a process that relays its stdio to a unix socket — the
// multiturn command re-invoked with --bridge.
func NewCLIAgent(label, modelID string, bridge []string) *CLIAgent {
	bin := os.Getenv("EVAL_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	return &CLIAgent{label: label, modelID: modelID, bin: bin, bridge: bridge, timeout: 15 * time.Minute}
}

// HostInjectsInstructions tells the runner Claude Code delivers the server's
// instructions itself.
func (a *CLIAgent) HostInjectsInstructions() bool { return true }

func (a *CLIAgent) Label() string   { return a.label }
func (a *CLIAgent) Backend() string { return "cli" }
func (a *CLIAgent) ModelID() string { return a.modelID }

// mcpServerName is the name the host sees; Claude Code exposes the tools as
// mcp__<name>__<tool>, and Episode.Call records the bare tool name.
const mcpServerName = "eval"

func (a *CLIAgent) Run(ctx context.Context, ep *Episode) error {
	// Unix socket paths are capped near 104 bytes; a long TMPDIR (a sandbox,
	// a CI runner) would overflow it, so fall back to /tmp.
	tmp := os.TempDir()
	if len(tmp) > 60 {
		tmp = "/tmp"
	}
	dir, err := os.MkdirTemp(tmp, "eval-cli-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	sock := filepath.Join(dir, "surface.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer ln.Close()
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				ss, err := ServeSurface(ep).Connect(serveCtx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
				if err != nil {
					_ = conn.Close()
					return
				}
				_ = ss.Wait()
			}()
		}
	}()

	cfg := map[string]any{"mcpServers": map[string]any{mcpServerName: map[string]any{
		"type": "stdio", "command": a.bridge[0], "args": append(append([]string(nil), a.bridge[1:]...), sock),
	}}}
	cfgPath := filepath.Join(dir, "mcp.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return err
	}

	// The CLI reports turns only in aggregate, after the fact: calls made
	// through the bridge record turn -1 (unknown), not a misleading 0. The
	// episode's own count goes back to 0 unless the CLI reports one, so a
	// failed run never drags a turns average negative.
	ep.Turns = -1
	defer func() {
		if ep.Turns < 0 {
			ep.Turns = 0
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, a.bin,
		"-p",
		"--model", a.modelID,
		"--output-format", "json",
		"--system-prompt", ep.System,
		"--tools", "",
		"--strict-mcp-config", "--mcp-config", cfgPath,
		"--allowedTools", "mcp__"+mcpServerName,
		"--setting-sources", "",
		"--no-session-persistence",
		"--max-turns", fmt.Sprint(ep.MaxTurns),
	)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(ep.Task.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	var out struct {
		Result     string `json:"result"`
		IsError    bool   `json:"is_error"`
		Subtype    string `json:"subtype"`
		NumTurns   int    `json:"num_turns"`
		ModelUsage map[string]struct {
			InputTokens              int     `json:"inputTokens"`
			OutputTokens             int     `json:"outputTokens"`
			CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
			CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
			CostUSD                  float64 `json:"costUSD"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		if runErr != nil {
			return fmt.Errorf("claude cli: %w: %s", runErr, truncate(strings.TrimSpace(stderr.String()), 500))
		}
		return fmt.Errorf("claude cli: decode output: %w", err)
	}
	ep.Turns = out.NumTurns
	ep.cliCost = 0
	for _, u := range out.ModelUsage {
		ep.Usage.add(Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens})
		ep.cliCost += u.CostUSD
	}
	ep.hasCLICost = true
	if out.Subtype == "error_max_turns" {
		ep.Exhausted = true
		return nil
	}
	if out.IsError {
		return fmt.Errorf("claude cli reported error (%s): %s", out.Subtype, truncate(out.Result, 500))
	}
	if runErr != nil {
		// Parseable output does not make a failed host process a success.
		return fmt.Errorf("claude cli: %w: %s", runErr, truncate(strings.TrimSpace(stderr.String()), 500))
	}
	ep.Finish(out.Result)
	return nil
}

// ServeSurface builds an MCP server that presents an episode's surface — the
// arm's instructions and tools — and answers each call through Episode.Call,
// one at a time so backend exchanges attribute to the call that made them.
func ServeSurface(ep *Episode) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: ep.Surface.ServerName, Version: "eval"},
		&mcp.ServerOptions{Instructions: ep.Surface.Instructions})
	var mu sync.Mutex
	for _, t := range ep.Surface.Tools {
		name := t.Name
		srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "invalid arguments: " + err.Error()}}}, nil
				}
			}
			if args == nil {
				args = map[string]any{}
			}
			mu.Lock()
			defer mu.Unlock()
			text, isErr := ep.Call(ctx, name, args)
			return &mcp.CallToolResult{IsError: isErr, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
	}
	return srv
}

// Bridge relays stdio to a unix socket until either side closes: the process
// the CLI spawns as its MCP server.
func Bridge(sock string, stdin io.Reader, stdout io.Writer) error {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan error, 2)
	go func() { _, err := io.Copy(conn, stdin); done <- err }()
	go func() { _, err := io.Copy(stdout, conn); done <- err }()
	return <-done
}
