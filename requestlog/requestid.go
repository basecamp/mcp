package requestlog

import (
	"context"
	"net/http"
)

// Header carries a request's id: kamal-proxy sets it on every request it
// forwards that arrives without one (a UUID), and keeps one that arrives
// with it, so a client may choose its own.
const Header = "X-Request-ID"

type requestIDKey struct{}

// WithRequestID returns ctx carrying id as its request's id, cut to
// MaxField bytes. A server that makes its own id when a request arrives
// without one sets it here, so the per-message line and the server's own
// lines name the same id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, truncate(id))
}

// RequestID is the id ctx's request carries, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Inbound gives each request's context the id its Header names, unless the
// context already carries one (a server that set its own, outside). A
// request without the header carries none: Inbound makes no id of its own.
func Inbound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get(Header); id != "" && RequestID(r.Context()) == "" {
			r = r.WithContext(WithRequestID(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

// Transport sends the id of the request an outbound call is made for, as
// Header, on every outbound request whose context carries one and that
// doesn't name an id of its own, so the app's log of the call joins the line
// of the MCP request that made it. base nil is http.DefaultTransport.
//
// It changes nothing else about the request: not its credentials, not its
// body. Several outbound calls made for one MCP request all carry its id.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base}
}

type transport struct {
	base http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	id := RequestID(req.Context())
	if id == "" || req.Header.Get(Header) != "" {
		return t.base.RoundTrip(req)
	}
	// A RoundTripper must not modify the request it was given.
	clone := req.Clone(req.Context())
	clone.Header.Set(Header, id)
	return t.base.RoundTrip(clone)
}
