package filecontent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	surfguard "github.com/basecamp/surfguard/go"
)

// OpenAIFileHosts are the hosts the production fetcher reads a file
// object's download_url from: the URL ChatGPT puts in it must name one of
// them exactly, over https on the default port, and so must every redirect.
//
// OpenAI documents no host for a fileParams download_url
// (developers.openai.com/plugins/reference, "Define file inputs", read
// 2026-11-23). files.oaiusercontent.com is the host of every example
// download link in OpenAI's GPT Actions file docs
// (developers.openai.com/api/docs/actions/sending-files), a sibling
// feature, and of the sample fileParams payloads in
// github.com/openai/openai-apps-sdk-examples/issues/185. It is not yet
// confirmed by a real ChatGPT call: a refused host is logged by name
// (never the URL), so the first refusal in production says what to put
// here. Never a suffix match: oaiusercontent.com also serves third-party
// app widgets (web-sandbox.oaiusercontent.com).
func OpenAIFileHosts() []string { return []string{"files.oaiusercontent.com"} }

// FetchTimeout bounds one fetch of a download_url, from the first dial to
// the last byte read.
const FetchTimeout = 60 * time.Second

// Fetcher fetches a download_url from its pinned hosts alone.
type Fetcher struct {
	// hosts are the allowed authorities, compared exactly after
	// lower-casing and dropping an explicit :443.
	hosts  []string
	policy surfguard.Policy
	client *http.Client
}

// NewFetcher builds a fetcher: policy judges every address dialed
// (surfguard: no loopback, private, link-local or special-use address, on
// any hop), roots are the trusted CAs (nil: the system's), and every request,
// the first and each redirect, must be https to one of hosts. No cookies or
// credentials are sent: the client has no jar and nothing sets a header.
//
// Production uses OpenAIFetcher; a test pins its own TLS server's host
// under policy.AllowLoopback().AllowAllPorts().
func NewFetcher(hosts []string, policy surfguard.Policy, roots *x509.CertPool) *Fetcher {
	f := &Fetcher{hosts: slices.Clone(hosts), policy: policy}
	transport := policy.Transport()
	transport.ResponseHeaderTimeout = 30 * time.Second
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	f.client = &http.Client{
		Transport: pinnedRoundTripper{pin: f, next: transport},
		CheckRedirect: policy.MaxRedirects(3).CheckRedirect(func(req *http.Request, _ []*http.Request) error {
			if !f.Allows(req.URL) {
				return errOffHost
			}
			return nil
		}),
		Timeout: FetchTimeout,
	}
	return f
}

var openAIFetcher = sync.OnceValue(func() *Fetcher {
	return NewFetcher(OpenAIFileHosts(), ProductionPolicy(), nil)
})

// OpenAIFetcher is the production fetcher: OpenAIFileHosts under
// ProductionPolicy, with the system's CAs.
func OpenAIFetcher() *Fetcher { return openAIFetcher() }

// ProductionPolicy is the production fetcher's address policy: surfguard's,
// with IANA's special-use ranges refused too, on ports 80 and 443 (https
// alone passes the pin).
func ProductionPolicy() surfguard.Policy { return surfguard.Policy{}.IANASpecialUse() }

// Hosts are the fetcher's pinned hosts.
func (f *Fetcher) Hosts() []string { return slices.Clone(f.hosts) }

// Client is the pinned client, for a test that shows the policy refusing
// what a test pin admits. Its requests carry no credentials.
func (f *Fetcher) Client() *http.Client { return f.client }

// Allows reports whether u is https to one of the pinned hosts.
func (f *Fetcher) Allows(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" {
		return false
	}
	return slices.Contains(f.hosts, strings.TrimSuffix(strings.ToLower(u.Host), ":443"))
}

var errOffHost = errors.New("download_url is not on OpenAI's file host")

// budgetSpent refuses a file call while other calls hold the budget. "Rate
// limited" opens it so a protocol layer can read it as the caller's to retry.
const budgetSpent = "Rate limited: the server is reading other files right now; try again in a few seconds"

// pinnedRoundTripper refuses any request off the pin, the first and every
// redirect alike, before it is dialed.
type pinnedRoundTripper struct {
	pin  *Fetcher
	next http.RoundTripper
}

func (r pinnedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || !r.pin.Allows(req.URL) {
		return nil, errOffHost
	}
	return r.next.RoundTrip(req)
}

func (r pinnedRoundTripper) CloseIdleConnections() {
	if c, ok := r.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// fetch reads a file object's bytes from its download_url, refusing any URL
// off the pin and any file larger than opts.MaxSize (counted as the body
// streams, whatever Content-Length says). The URL is a bearer capability for
// the person's file: no message, log line or error built here carries it,
// only, on a refusal, the host it named.
//
// The bytes are counted against opts.Budget from before the read until the
// caller calls the returned release, so a burst of small calls can't each
// make the server hold a large file at once: a call that finds the budget
// spent is refused, to try again. A refusal's release is a no-op.
func (f *Fetcher) fetch(ctx context.Context, raw any, opts Options) (data []byte, release func(), err error) {
	release = func() {}
	maxSize := opts.MaxSize
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, release, invalid("file must be the file object ChatGPT passes for an attached file")
	}
	downloadURL, ok := obj["download_url"].(string)
	if !ok || strings.TrimSpace(downloadURL) == "" {
		return nil, release, missing("file.download_url")
	}
	logger := opts.logger()
	u, parseErr := url.Parse(downloadURL)
	if parseErr != nil || !f.Allows(u) {
		host := ""
		if u != nil {
			host = u.Host
		}
		logger.Warn("file download_url refused: not OpenAI's file host", "host", host)
		return nil, release, invalid("file.download_url must be on OpenAI's file host; attach the file in ChatGPT and call again")
	}
	budget := opts.Budget
	if budget == nil {
		logger.Error("file download_url refused: no download budget configured")
		return nil, release, &Error{Kind: Busy, Message: budgetSpent}
	}

	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if reqErr != nil {
		return nil, release, invalid("file.download_url can't be read")
	}
	resp, doErr := f.client.Do(req)
	if doErr != nil {
		// doErr is a *url.Error, whose text carries the URL: never returned
		// or logged as is.
		var violation *surfguard.Violation
		switch {
		case errors.Is(doErr, errOffHost):
			logger.Warn("file download_url redirected off OpenAI's file host")
			return nil, release, invalid("file.download_url redirected away from OpenAI's file host, so nothing was uploaded")
		case errors.As(doErr, &violation):
			logger.Warn("file download_url refused by the address policy", "reason", violation.Reason.String())
			return nil, release, invalid("file.download_url can't be fetched from this server, so nothing was uploaded")
		case errors.Is(doErr, context.DeadlineExceeded) || isTimeout(doErr):
			return nil, release, invalid("reading the attached file from OpenAI timed out, so nothing was uploaded; try again")
		default:
			logger.Warn("file download_url fetch failed")
			return nil, release, invalid("the attached file couldn't be read from OpenAI, so nothing was uploaded; attach it again and retry")
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, release, invalid("OpenAI's file host answered HTTP %d for the attached file, so nothing was uploaded; its link may have expired, so attach it again and retry", resp.StatusCode)
	}
	if resp.ContentLength > maxSize {
		return nil, release, &Error{Kind: TooLarge, Message: fmt.Sprintf("file too large: %d bytes exceeds maximum of %d bytes", resp.ContentLength, maxSize)}
	}
	// Reserve what the file may hold: its declared length, or the cap when
	// it declares none, never more than the whole budget. Clamped before
	// adding one, so a cap configured near math.MaxInt64 can't overflow.
	reserve := min(maxSize, budget.Size()-1)
	if resp.ContentLength >= 0 {
		reserve = min(reserve, resp.ContentLength)
	}
	reserve++
	held, ok := budget.Reserve(reserve)
	if !ok {
		return nil, release, &Error{Kind: Busy, Message: budgetSpent}
	}
	// The buffer is exactly the reservation, so what the process holds is
	// what the budget counts: io.ReadAll's doubling would hold up to twice
	// a file's size, and a small file a buffer larger than its reservation.
	// Only the body's own EOF ends a read: a response cut short of its
	// Content-Length, or a broken chunked one, is an error, never a file.
	buf := make([]byte, reserve)
	var n int
	var readErr error
	for n < len(buf) && readErr == nil {
		var m int
		m, readErr = resp.Body.Read(buf[n:])
		n += m
	}
	data = buf[:n]
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil {
		held()
		if errors.Is(readErr, context.DeadlineExceeded) || isTimeout(readErr) {
			return nil, release, invalid("reading the attached file from OpenAI timed out, so nothing was uploaded; try again")
		}
		return nil, release, invalid("the attached file couldn't be read from OpenAI, so nothing was uploaded; attach it again and retry")
	}
	if int64(len(data)) >= reserve {
		// Only a body of undeclared length gets here: net/http ends a
		// declared one at its Content-Length.
		held()
		return nil, release, &Error{Kind: TooLarge, Message: fmt.Sprintf("file too large: more than the maximum of %d bytes", reserve-1)}
	}
	return data, held, nil
}

// isTimeout reports whether err is a network timeout.
func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}
