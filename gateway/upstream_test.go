package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/gateway"
)

// leakyErr is an upstream error as a client builds it: it carries the
// method, URL, response body and Go error text that must never reach the
// person.
func leakyErr(cause error) error {
	return fmt.Errorf("POST /897362094/cards/42/comments failed: <html>secret body</html>: %w",
		&url.Error{Op: "Post", URL: "https://app.example.com/897362094/cards/42/comments", Err: cause})
}

// leakMarkers are the fragments of leakyErr that must not appear in a result.
var leakMarkers = []string{"POST", "Post", "/897362094", "cards/42", "app.example.com", "secret body", "<html>", "failed:", "dial tcp", "connection reset", "i/o timeout"}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res)
	require.True(t, res.IsError)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

func dialErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
}

func readErr() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestUpstreamFailureWords(t *testing.T) {
	status := func(code int) error { return fmt.Errorf("fizzy returned %d: <html>secret body</html>", code) }
	cases := []struct {
		name string
		err  error
		call gateway.Upstream
		want string
	}{
		{"read 500", status(500), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead, Status: 500, RequestID: "req-1"},
			"Error: Fizzy hit an error on its side (HTTP 500) and didn't complete this request (request_id: req-1)\nNext: Try again later."},
		{"write 502", status(502), gateway.Upstream{Product: "HEY", Kind: gateway.KindWrite, Status: 502},
			"Error: HEY hit an error on its side (HTTP 502) before confirming this change, so it may or may not have been made\nNext: Check in HEY whether the change went through before trying again, so it isn't made twice."},
		{"send 503", status(503), gateway.Upstream{Product: "HEY", Kind: gateway.KindSend, Status: 503, RequestID: "abc"},
			"Error: HEY hit an error on its side (HTTP 503) before confirming the send, so it may or may not have been sent (request_id: abc)\nNext: Check in HEY whether it went out before sending it again, so it isn't sent twice."},
		{"read 504 is a timeout", status(504), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead, Status: 504},
			"Error: Fizzy didn't answer in time (HTTP 504), so this request wasn't completed\nNext: Try again later."},
		{"write 504 is a timeout", status(504), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindWrite, Status: 504},
			"Error: Fizzy didn't answer in time (HTTP 504), so this change may or may not have been made\nNext: Check in Fizzy whether the change went through before trying again, so it isn't made twice."},
		{"read deadline", leakyErr(context.DeadlineExceeded), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead},
			"Error: Fizzy didn't answer in time, so this request wasn't completed\nNext: Try again later."},
		{"read net timeout", leakyErr(timeoutErr{}), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead},
			"Error: Fizzy didn't answer in time, so this request wasn't completed\nNext: Try again later."},
		{"send timeout", leakyErr(timeoutErr{}), gateway.Upstream{Product: "HEY", Kind: gateway.KindSend},
			"Error: HEY didn't answer in time, so it may or may not have been sent\nNext: Check in HEY whether it went out before sending it again, so it isn't sent twice."},
		{"read dial", leakyErr(dialErr()), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead},
			"Error: Fizzy couldn't be reached, so this request wasn't completed\nNext: Try again later."},
		{"read dns", leakyErr(&net.DNSError{Err: "no such host", Name: "app.example.com"}), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead},
			"Error: Fizzy couldn't be reached, so this request wasn't completed\nNext: Try again later."},
		{"write dial may still have landed", leakyErr(dialErr()), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindWrite},
			"Error: The connection to Fizzy failed before it answered, so this change may or may not have been made\nNext: Check in Fizzy whether the change went through before trying again, so it isn't made twice."},
		{"read reset", leakyErr(readErr()), gateway.Upstream{Product: "HEY", Kind: gateway.KindRead},
			"Error: The connection to HEY failed, so this request wasn't completed\nNext: Try again later."},
		{"read body cut short", leakyErr(io.ErrUnexpectedEOF), gateway.Upstream{Product: "HEY", Kind: gateway.KindRead},
			"Error: The connection to HEY failed, so this request wasn't completed\nNext: Try again later."},
		{"send reset", leakyErr(readErr()), gateway.Upstream{Product: "HEY", Kind: gateway.KindSend},
			"Error: The connection to HEY failed before it answered, so it may or may not have been sent\nNext: Check in HEY whether it went out before sending it again, so it isn't sent twice."},
		{"200 whose body was cut short", leakyErr(io.ErrUnexpectedEOF), gateway.Upstream{Product: "HEY", Kind: gateway.KindWrite, Status: 200},
			"Error: The connection to HEY failed before it answered, so this change may or may not have been made\nNext: Check in HEY whether the change went through before trying again, so it isn't made twice."},
		{"200 whose body timed out", leakyErr(timeoutErr{}), gateway.Upstream{Product: "HEY", Kind: gateway.KindRead, Status: 200},
			"Error: HEY didn't answer in time, so this request wasn't completed\nNext: Try again later."},
		{"network vouched by the client", errors.New("Network error: transport failure"), gateway.Upstream{Product: "HEY", Kind: gateway.KindRead, Network: true},
			"Error: The connection to HEY failed, so this request wasn't completed\nNext: Try again later."},
		{"next replaced", status(500), gateway.Upstream{Product: "HEY", Kind: gateway.KindSend, Status: 500, Next: "Check Sent in HEY; this draft won't be sent again."},
			"Error: HEY hit an error on its side (HTTP 500) before confirming the send, so it may or may not have been sent\nNext: Check Sent in HEY; this draft won't be sent again."},
		{"zero kind is a write", status(500), gateway.Upstream{Product: "Fizzy", Status: 500},
			"Error: Fizzy hit an error on its side (HTTP 500) before confirming this change, so it may or may not have been made\nNext: Check in Fizzy whether the change went through before trying again, so it isn't made twice."},
		{"401 opted in", status(401), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead, Status: 401, Refusals: true},
			"Error: Fizzy didn't accept this connection's credentials (HTTP 401)\nNext: Reconnect to Fizzy, or renew its access, then try again."},
		{"403 opted in", status(403), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindWrite, Status: 403, Refusals: true},
			"Error: Fizzy refused this request (HTTP 403): this connection isn't allowed to do it\nNext: Check this connection's access in Fizzy; trying again as it is won't help."},
		{"404 opted in", status(404), gateway.Upstream{Product: "HEY", Kind: gateway.KindRead, Status: 404, RequestID: "r9", Refusals: true},
			"Error: HEY couldn't find what this request names (HTTP 404) (request_id: r9)\nNext: Check the IDs in the request: what it names may have been deleted, or this account can't see it."},
		{"429 opted in", status(429), gateway.Upstream{Product: "HEY", Kind: gateway.KindRead, Status: 429, Refusals: true},
			"Error: HEY is limiting requests right now (HTTP 429)\nNext: Try again later."},
		{"other 4xx opted in", status(413), gateway.Upstream{Product: "Fizzy", Kind: gateway.KindWrite, Status: 413, Refusals: true},
			"Error: Fizzy refused this request (HTTP 413)\nNext: Check the request before trying again."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := resultText(t, gateway.UpstreamFailure(tc.err, tc.call))
			assert.Equal(t, tc.want, text)
			for _, marker := range leakMarkers {
				assert.NotContains(t, text, marker)
			}
		})
	}
}

// Anything that is not the product's or the network's failure is left to the
// caller.
func TestUpstreamFailureLeavesOthersToTheCaller(t *testing.T) {
	cases := []struct {
		name string
		err  error
		call gateway.Upstream
	}{
		{"no error", nil, gateway.Upstream{Product: "Fizzy", Status: 500}},
		{"caller left", leakyErr(context.Canceled), gateway.Upstream{Product: "Fizzy"}},
		{"caller left during a 5xx", context.Canceled, gateway.Upstream{Product: "Fizzy", Status: 502}},
		{"422 keeps its reasons", errors.New("Subject can't be blank"), gateway.Upstream{Product: "HEY", Status: 422}},
		{"404 not opted in", errors.New("not found"), gateway.Upstream{Product: "HEY", Status: 404}},
		{"3xx", errors.New("redirect"), gateway.Upstream{Product: "HEY", Status: 302, Refusals: true}},
		// http.Client wraps whatever a RoundTripper returns; the server's own
		// transport failing before it sends is its failure, not the network's.
		{"own transport failed before sending", &url.Error{Op: "Get", URL: "https://x", Err: errors.New("mint proof: key missing")}, gateway.Upstream{Product: "Fizzy"}},
		{"plain internal error", errors.New("encode request body: unsupported type"), gateway.Upstream{Product: "Fizzy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Nil(t, gateway.UpstreamFailure(tc.err, tc.call))
		})
	}
}

// Real failures from net/http classify as the constructed ones above do.
func TestUpstreamFailureFromHTTPClient(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	_, err = http.Get("http://" + addr + "/897362094/cards") //nolint:noctx // a refused dial needs no context
	require.Error(t, err)
	text := resultText(t, gateway.UpstreamFailure(err, gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead}))
	assert.Equal(t, "Error: Fizzy couldn't be reached, so this request wasn't completed\nNext: Try again later.", text)
	assert.NotContains(t, text, addr)

	// A connection the server closes without answering.
	hangup, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hangup.Close() })
	go func() {
		for {
			conn, err := hangup.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 1024)
			_, _ = conn.Read(buf)
			_ = conn.Close()
		}
	}()
	req, err := http.NewRequest(http.MethodPost, "http://"+hangup.Addr().String()+"/cards", nil)
	require.NoError(t, err)
	_, err = http.DefaultClient.Do(req)
	require.Error(t, err)
	text = resultText(t, gateway.UpstreamFailure(err, gateway.Upstream{Product: "Fizzy", Kind: gateway.KindWrite}))
	assert.Equal(t, "Error: The connection to Fizzy failed before it answered, so this change may or may not have been made\nNext: Check in Fizzy whether the change went through before trying again, so it isn't made twice.", text)

	// A deadline that passes while the server holds the request.
	slow, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = slow.Close() })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		for {
			conn, err := slow.Accept()
			if err != nil {
				return
			}
			go func() { <-release; _ = conn.Close() }()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "http://"+slow.Addr().String()+"/cards", nil)
	require.NoError(t, err)
	_, err = http.DefaultClient.Do(req)
	require.Error(t, err)
	text = resultText(t, gateway.UpstreamFailure(err, gateway.Upstream{Product: "Fizzy", Kind: gateway.KindRead}))
	assert.Equal(t, "Error: Fizzy didn't answer in time, so this request wasn't completed\nNext: Try again later.", text)
}

func TestKindOf(t *testing.T) {
	assert.Equal(t, gateway.KindRead, gateway.KindOf(gateway.Operation{Action: "list", ReadOnly: true}))
	assert.Equal(t, gateway.KindWrite, gateway.KindOf(gateway.Operation{Action: "create"}))
}
