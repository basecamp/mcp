package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Upstream failures: the product's API failed on its side (a 5xx), didn't
// answer in time, or couldn't be reached. UpstreamFailure answers one in the
// server's own words, so the person learns that the product was the one
// failing, whether anything may have changed, and that trying again later can
// work. Nothing the product or the network wrote reaches the text: no
// response body, URL, path, method, host or Go error string. Only the status,
// a fixed sentence, a next step and the product's request ID do.
//
// What it says about a write follows what can be known. A 5xx can come after
// the product committed the write, and a timeout or a connection lost
// mid-request is no answer at all, so for any of these a write says the
// change may or may not have been made, and to check before trying again.
// Even a connection that never opened may be a redirect's, after the request
// it followed was taken, so a write never claims nothing landed.
//
// basecamp-mcp-server#416 wrote these words first, for Basecamp; this is
// their product-neutral form.

// Kind is what a failed call does: read, change, or send.
type Kind int

const (
	// KindWrite changes data. The zero Kind, and the cautious answer: the
	// change may or may not have been made.
	KindWrite Kind = iota
	// KindRead only reads, so trying again is always safe.
	KindRead
	// KindSend sends something on the person's behalf (an email, a report),
	// which may or may not have gone out.
	KindSend
)

// KindOf is an operation's Kind by whether it only reads.
func KindOf(op Operation) Kind {
	if op.ReadOnly {
		return KindRead
	}
	return KindWrite
}

// Upstream is one failed call to a product's API, as its client saw it.
type Upstream struct {
	// Product names the API in the text, e.g. "Fizzy" or "HEY".
	Product string
	// Kind is what the call does.
	Kind Kind
	// Status is the HTTP status the product answered, 0 when it didn't.
	Status int
	// RequestID is the product's request ID for the response, when it sent
	// one. It is served as given.
	RequestID string
	// Network is set when the caller's client reports that the transport
	// failed while reaching the product (a connection, DNS, TLS or timeout
	// failure the client wrapped), for a failure the error chain doesn't
	// name itself. Leave it unset for anything that fails before sending,
	// such as the server's own credential lookup: those are the server's
	// failures, and UpstreamFailure answers them only when the chain names
	// a network failure.
	Network bool
	// Next replaces the default next step, for a caller that knows more
	// (a send its own guard won't repeat, say). It is served as given.
	Next string
	// Refusals answers a 4xx too, in fixed words naming its status. Unset,
	// a 4xx is left to the caller, whose text may carry the product's own
	// words for the person (a validation failure's reasons). Set it for a
	// status whose text the caller shouldn't serve.
	Refusals bool
}

// failure is the class of an upstream failure.
type failure int

const (
	notUpstream failure = iota
	serverError
	timeout
	unreachable
	connectionLost
	refusal
)

// UpstreamFailure answers err, a failed call to the product's API, as an
// in-band isError result in the server's own words, or returns nil when err
// is not an upstream failure: a cancellation (the caller left), a 4xx
// without Refusals, or an error that is neither the product's nor the
// network's, which the caller answers as before.
//
// The text is "Error: <what happened>[ (request_id: <id>)]\nNext: <step>",
// the shape basecamp-mcp-server serves.
func UpstreamFailure(err error, call Upstream) *mcp.CallToolResult {
	class := classifyUpstream(err, call)
	if class == notUpstream {
		return nil
	}
	what, next := upstreamWords(class, call)
	if call.Next != "" {
		next = call.Next
	}
	text := "Error: " + what
	if call.RequestID != "" {
		text += " (request_id: " + call.RequestID + ")"
	}
	return ErrorResult("%s", text+"\nNext: "+next)
}

// classifyUpstream names err's class of upstream failure.
func classifyUpstream(err error, call Upstream) failure {
	if err == nil || errors.Is(err, context.Canceled) {
		return notUpstream
	}
	switch status := call.Status; {
	case status == http.StatusGatewayTimeout:
		// The proxy in front of the product gave up waiting; the product
		// may still have done the work.
		return timeout
	case status >= 500 && status < 600:
		return serverError
	case status >= 400 && status < 500:
		if call.Refusals {
			return refusal
		}
		return notUpstream
	case status != 0:
		return notUpstream
	}

	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr), errors.As(err, &opErr) && opErr.Op == "dial":
		return unreachable
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		return timeout
	case isConnectionFailure(err), call.Network:
		return connectionLost
	}
	return notUpstream
}

// isNetTimeout reports whether err is a network timeout.
func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isConnectionFailure reports whether err is the network failing under a
// request: a network operation error, a connection reset or closed early, or
// a body cut short. A *url.Error alone is not enough: http.Client wraps
// whatever a RoundTripper returns, including a server's own transport
// failing before it sends.
func isConnectionFailure(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}

// upstreamWords is what happened and the next step, for class and call.
func upstreamWords(class failure, call Upstream) (what, next string) {
	p := call.Product
	status := ""
	if call.Status != 0 {
		status = fmt.Sprintf(" (HTTP %d)", call.Status)
	}
	if class == refusal {
		return refusalWords(p, call.Status)
	}

	switch call.Kind {
	case KindRead:
		next = "Try again later."
		switch class {
		case serverError:
			what = fmt.Sprintf("%s hit an error on its side%s and didn't complete this request", p, status)
		case timeout:
			what = fmt.Sprintf("%s didn't answer in time%s, so this request wasn't completed", p, status)
		case unreachable:
			what = fmt.Sprintf("%s couldn't be reached, so this request wasn't completed", p)
		default:
			what = fmt.Sprintf("The connection to %s failed, so this request wasn't completed", p)
		}
	case KindSend:
		next = fmt.Sprintf("Check in %s whether it went out before sending it again, so it isn't sent twice.", p)
		switch class {
		case serverError:
			what = fmt.Sprintf("%s hit an error on its side%s before confirming the send, so it may or may not have been sent", p, status)
		case timeout:
			what = fmt.Sprintf("%s didn't answer in time%s, so it may or may not have been sent", p, status)
		default:
			what = fmt.Sprintf("The connection to %s failed before it answered, so it may or may not have been sent", p)
		}
	default:
		next = fmt.Sprintf("Check in %s whether the change went through before trying again, so it isn't made twice.", p)
		switch class {
		case serverError:
			what = fmt.Sprintf("%s hit an error on its side%s before confirming this change, so it may or may not have been made", p, status)
		case timeout:
			what = fmt.Sprintf("%s didn't answer in time%s, so this change may or may not have been made", p, status)
		default:
			what = fmt.Sprintf("The connection to %s failed before it answered, so this change may or may not have been made", p)
		}
	}
	return what, next
}

// refusalWords is what a 4xx says, by status alone.
func refusalWords(p string, status int) (what, next string) {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Sprintf("%s didn't accept this connection's credentials (HTTP 401)", p),
			fmt.Sprintf("Reconnect to %s, or renew its access, then try again.", p)
	case http.StatusForbidden:
		return fmt.Sprintf("%s refused this request (HTTP 403): this connection isn't allowed to do it", p),
			fmt.Sprintf("Check this connection's access in %s; trying again as it is won't help.", p)
	case http.StatusNotFound:
		return fmt.Sprintf("%s couldn't find what this request names (HTTP 404)", p),
			"Check the IDs in the request: what it names may have been deleted, or this account can't see it."
	case http.StatusTooManyRequests:
		return fmt.Sprintf("%s is limiting requests right now (HTTP 429)", p),
			"Try again later."
	default:
		return fmt.Sprintf("%s refused this request (HTTP %d)", p, status),
			"Check the request before trying again."
	}
}
