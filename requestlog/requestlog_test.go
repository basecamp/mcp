package requestlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sentinels the wire test sends in every place a request or its answer
// carries content; none may reach the log.
const (
	logArgument = "argument-sentinel-91c2"
	logOutput   = "output-sentinel-4be0"
	logError    = "error-sentinel-d81f"
)

// syncBuffer is a log sink the handler's goroutines can write while the
// test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// requestLines are the Message lines logged so far, decoded.
func requestLines(t *testing.T, b *syncBuffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(b.String()))
	for scanner.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
		if line["msg"] == Message {
			lines = append(lines, line)
		}
	}
	return lines
}

type grantKey struct{}

// newLoggingServer serves two tools statelessly, as the hosted servers do,
// with Install outermost and Inbound on the HTTP handler. Its echo tool
// answers with logOutput; its fail tool with an error result quoting
// logError. A request's grant is its X-Test-Grant header.
func newLoggingServer(t *testing.T) (*httptest.Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	type args struct {
		Text string `json:"text"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: logOutput}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "fail"}, func(context.Context, *mcp.CallToolRequest, args) (*mcp.CallToolResult, any, error) {
		return nil, nil, fmt.Errorf("upstream said %s", logError)
	})
	Install(server, logger, Options{
		Tools: []string{"echo", "fail"},
		Grant: func(ctx context.Context) string { id, _ := ctx.Value(grantKey{}).(string); return id },
	})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	withGrant := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g := r.Header.Get("X-Test-Grant"); g != "" {
			r = r.WithContext(context.WithValue(r.Context(), grantKey{}, g))
		}
		sdk.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(Inbound(withGrant))
	t.Cleanup(ts.Close)
	return ts, logs
}

// idSender numbers each HTTP request it sends with an X-Request-ID of its
// own and remembers the ids, in order.
type idSender struct {
	base http.RoundTripper
	mu   sync.Mutex
	sent []string
}

func (s *idSender) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	id := fmt.Sprintf("rid-%d-%s", len(s.sent)+1, strings.Repeat("x", 8))
	s.sent = append(s.sent, id)
	s.mu.Unlock()
	clone := req.Clone(req.Context())
	clone.Header.Set(Header, id)
	clone.Header.Set("X-Test-Grant", "grant-1")
	return s.base.RoundTrip(clone)
}

func TestEveryRequestIsLoggedOnceWithoutItsContent(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-11-25", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			ts, logs := newLoggingServer(t)
			sender := &idSender{base: ts.Client().Transport}
			client := mcp.NewClient(&mcp.Implementation{Name: "log-test", Version: "1.2.3"}, nil)
			session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint:             ts.URL,
				HTTPClient:           &http.Client{Transport: sender},
				DisableStandaloneSSE: true,
			}, &mcp.ClientSessionOptions{ProtocolVersion: version})
			require.NoError(t, err)
			defer func() { _ = session.Close() }()

			_, err = session.ListTools(context.Background(), nil)
			require.NoError(t, err)
			echo, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "echo", Arguments: map[string]any{"text": logArgument},
			})
			require.NoError(t, err)
			require.Contains(t, echo.Content[0].(*mcp.TextContent).Text, logOutput, "the output sentinel reached the client")
			failed, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "fail", Arguments: map[string]any{"text": logArgument},
			})
			require.NoError(t, err)
			require.True(t, failed.IsError)
			_, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "made_up_" + logArgument})
			require.Error(t, err)

			raw := logs.String()
			for _, sentinel := range []string{logArgument, logOutput, logError} {
				assert.NotContains(t, raw, sentinel)
			}

			sender.mu.Lock()
			sent := append([]string(nil), sender.sent...)
			sender.mu.Unlock()
			byMethod := map[string][]map[string]any{}
			var logged []string
			for _, line := range requestLines(t, logs) {
				method, _ := line["method"].(string)
				byMethod[method] = append(byMethod[method], line)
				assert.Contains(t, line, "duration_ms", method)
				assert.Equal(t, "grant-1", line["grant"], method)
				assert.Contains(t, line, "result", method)
				id, _ := line["request_id"].(string)
				assert.Contains(t, sent, id, "%s: request_id is the X-Request-ID its HTTP request carried", method)
				logged = append(logged, id)
			}
			// Each line names its own request's id, not another's.
			seen := map[string]bool{}
			for _, id := range logged {
				assert.False(t, seen[id], "request_id %s logged twice", id)
				seen[id] = true
			}
			assert.Len(t, byMethod["tools/list"], 1)

			list := byMethod["tools/list"][0]
			assert.Equal(t, version, list["protocol"])
			assert.Equal(t, map[string]any{"status": "ok"}, list["result"])
			assert.Equal(t, "Go-http-client/1.1", list["user_agent"])

			type call struct{ tool, status string }
			var calls []call
			for _, line := range byMethod["tools/call"] {
				result, _ := line["result"].(map[string]any)
				tool, _ := line["tool"].(string)
				status, _ := result["status"].(string)
				calls = append(calls, call{tool, status})
			}
			assert.Equal(t, []call{{"echo", "ok"}, {"fail", "tool_error"}, {UnknownTool, "error"}}, calls)
			unknown, ok := byMethod["tools/call"][2]["result"].(map[string]any)
			require.True(t, ok)
			assert.Contains(t, unknown, "code")

			if version >= "2026-07-28" {
				// Each request names its client in _meta; there is no initialize.
				assert.Equal(t, "log-test", list["client"])
				assert.Equal(t, "1.2.3", list["client_version"])
				return
			}
			require.Len(t, byMethod["initialize"], 1)
			init := byMethod["initialize"][0]
			assert.Equal(t, version, init["protocol"])
			assert.Equal(t, "log-test", init["client"])
			assert.Equal(t, "1.2.3", init["client_version"])
			assert.Equal(t, "none", init["session"])
		})
	}
}

// call runs one tools/call of tool through the middleware over next, as a
// request carrying header, and returns the line it logged.
func call(t *testing.T, opts Options, ctx context.Context, header http.Header, next mcp.MethodHandler) map[string]any {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	handler := Middleware(logger, opts)(next)
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "get_me", Arguments: json.RawMessage(`{"q":"` + logArgument + `"}`)},
		Extra:  &mcp.RequestExtra{Header: header},
	}
	_, _ = handler(ctx, "tools/call", req)
	assert.NotContains(t, logs.String(), logArgument)
	lines := requestLines(t, logs)
	require.Len(t, lines, 1)
	return lines[0]
}

func answer(res *mcp.CallToolResult, err error) mcp.MethodHandler {
	return func(context.Context, string, mcp.Request) (mcp.Result, error) { return res, err }
}

var served = Options{Tools: []string{"get_me"}}

// A tool cut short answers with an error result and no error: by a
// cancellation, or by the server's own cut (CutShort). The log says
// canceled, not tool_error.
func TestCutShortCallIsLoggedAsCanceled(t *testing.T) {
	failed := answer(&mcp.CallToolResult{IsError: true}, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	spent := Options{Tools: served.Tools, CutShort: func(context.Context) bool { return true }}
	left := Options{Tools: served.Tools, CutShort: func(context.Context) bool { return false }}

	assert.Equal(t, map[string]any{"status": "canceled"}, call(t, served, cancelled, nil, failed)["result"], "cancelled")
	assert.Equal(t, map[string]any{"status": "canceled"}, call(t, spent, context.Background(), nil, failed)["result"], "cut short")
	assert.Equal(t, map[string]any{"status": "tool_error"}, call(t, left, context.Background(), nil, failed)["result"], "not cut short")
	assert.Equal(t, map[string]any{"status": "tool_error"}, call(t, served, context.Background(), nil, failed)["result"], "no CutShort")
	assert.Equal(t, "get_me", call(t, served, cancelled, nil, failed)["tool"])
}

// A question back to the client is input_required, inside the middleware
// chain where the SDK hasn't labelled the result yet.
func TestQuestionIsLoggedAsInputRequired(t *testing.T) {
	asks := answer(&mcp.CallToolResult{RequestState: "state"}, nil)
	assert.Equal(t, map[string]any{"status": "input_required"}, call(t, served, context.Background(), nil, asks)["result"])
	asksInput := answer(&mcp.CallToolResult{InputRequests: mcp.InputRequestMap{}}, nil)
	assert.Equal(t, map[string]any{"status": "input_required"}, call(t, served, context.Background(), nil, asksInput)["result"])
}

func TestRequestIDComesFromTheContextElseTheHeader(t *testing.T) {
	ok := answer(&mcp.CallToolResult{}, nil)
	header := http.Header{}
	header.Set(Header, "from-header")

	assert.Equal(t, "from-header", call(t, served, context.Background(), header, ok)["request_id"])
	// A server that made its own id names it on the context; that wins.
	ctx := WithRequestID(context.Background(), "from-server")
	assert.Equal(t, "from-server", call(t, served, ctx, header, ok)["request_id"])
	assert.NotContains(t, call(t, served, context.Background(), http.Header{}, ok), "request_id")
	assert.NotContains(t, call(t, served, context.Background(), nil, ok), "request_id")
}

func TestGrantIsWhatTheServerSays(t *testing.T) {
	ok := answer(&mcp.CallToolResult{}, nil)
	assert.NotContains(t, call(t, served, context.Background(), nil, ok), "grant")
	none := Options{Tools: served.Tools, Grant: func(context.Context) string { return "" }}
	assert.NotContains(t, call(t, none, context.Background(), nil, ok), "grant")
	some := Options{Tools: served.Tools, Grant: func(context.Context) string { return "g-7" }}
	assert.Equal(t, "g-7", call(t, some, context.Background(), nil, ok)["grant"])
}

func TestSessionSaysWhetherTheClientSentOne(t *testing.T) {
	ok := answer(&mcp.CallToolResult{}, nil)
	assert.Equal(t, "none", call(t, served, context.Background(), nil, ok)["session"])
	assert.Equal(t, "sent", call(t, served, context.Background(), http.Header{"Mcp-Session-Id": {"s"}}, ok)["session"])
}

func TestLoggedClientStringsAreBounded(t *testing.T) {
	long := strings.Repeat("é", MaxField) // two bytes a rune
	got := truncate(long)
	assert.LessOrEqual(t, len(got), MaxField)
	assert.True(t, strings.HasPrefix(long, got))
	assert.Equal(t, "short", truncate("short"))

	ok := answer(&mcp.CallToolResult{}, nil)
	header := http.Header{"User-Agent": {long}}
	header.Set(Header, long)
	line := call(t, served, context.Background(), header, ok)
	assert.Equal(t, got, line["user_agent"])
	assert.Equal(t, got, line["request_id"])
	assert.Equal(t, got, RequestID(WithRequestID(context.Background(), long)))

	init := &mcp.InitializeRequest{Params: &mcp.InitializeParams{
		ProtocolVersion: long, ClientInfo: &mcp.Implementation{Name: long, Version: long},
	}}
	logs := &syncBuffer{}
	handler := Middleware(slog.New(slog.NewJSONHandler(logs, nil)), served)(answer(nil, nil))
	_, _ = handler(context.Background(), "initialize", init)
	lines := requestLines(t, logs)
	require.Len(t, lines, 1)
	for _, key := range []string{"protocol", "client", "client_version"} {
		assert.Equal(t, got, lines[0][key], key)
	}
}
