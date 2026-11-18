package requestlog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboundTakesTheHeaderUnlessTheContextHasOne(t *testing.T) {
	var got string
	h := Inbound(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = RequestID(r.Context()) }))

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set(Header, "abc-123")
	h.ServeHTTP(httptest.NewRecorder(), r)
	assert.Equal(t, "abc-123", got)

	r = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set(Header, "abc-123")
	r = r.WithContext(WithRequestID(r.Context(), "made-here"))
	h.ServeHTTP(httptest.NewRecorder(), r)
	assert.Equal(t, "made-here", got)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	assert.Empty(t, got, "no header, no id: Inbound makes none")
}

func TestTransportSendsTheRequestsID(t *testing.T) {
	var seen []string
	app := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(Header)+"|"+r.Header.Get("Authorization"))
	}))
	defer app.Close()
	client := &http.Client{Transport: Transport(app.Client().Transport)}

	send := func(ctx context.Context, own string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, app.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer x")
		if own != "" {
			req.Header.Set(Header, own)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, own, req.Header.Get(Header), "the caller's request is left as it was")
	}
	ctx := WithRequestID(context.Background(), "rid-1")
	send(ctx, "")
	send(ctx, "its-own")
	send(context.Background(), "")

	assert.Equal(t, []string{"rid-1|Bearer x", "its-own|Bearer x", "|Bearer x"}, seen)
}

func TestTransportDefaultsToTheDefaultTransport(t *testing.T) {
	assert.Equal(t, http.DefaultTransport, Transport(nil).(*transport).base)
}
