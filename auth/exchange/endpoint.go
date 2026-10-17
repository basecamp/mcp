package exchange

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateEndpointURL enforces the transport rule for any endpoint that will
// carry credentials in request bodies (the exchange token URL, and a
// product's probes' equivalents): an absolute http(s) URL with a hostname, no
// userinfo, no fragment, and https unless the host is exact localhost or a
// loopback IP literal. Cleartext transport of the subject token and client
// credentials is a leak waiting for one typo'd scheme. New applies it;
// exported so a product's config load and probes apply the same rule.
//
// The same rule as basecamp-mcp-server's config.ValidateExchangeEndpointURL,
// with the same messages: none renders the raw value, which can carry
// user:pass@.
func ValidateEndpointURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		// Hostname(), not Host: a port-only authority like https://:443/
		// yields a nonempty Host and an empty Hostname, and would pass a
		// Host check only to fail every request at the transport layer.
		return errors.New("invalid exchange endpoint URL: must be an absolute http(s) URL with a hostname (value withheld from this error)")
	}
	// Userinfo is checked FIRST among the semantic rules and the raw URL is
	// never rendered below: a URL failing any later check while carrying
	// user:pass@ must not have the credential interpolated into the very
	// error that rejects it.
	if u.User != nil {
		// Fixed message: Redacted() preserves the username, which in the
		// token@host form IS the credential.
		return errors.New("exchange endpoint URL must not carry userinfo (value withheld from this error): " +
			"Go would send it as Basic authorization alongside the client credentials")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("%q must use https: requests to it carry credentials "+
			"(http is permitted only for exact localhost or a loopback IP literal - "+
			"*.localhost names are excluded because HTTP_PROXY would receive their cleartext)", u.Redacted())
	}
	// A fragment is never sent in an HTTP request, but private_key_jwt uses
	// the configured URL VERBATIM as its client-assertion audience — so
	// "https://as/token#x" would POST to /token with an assertion audienced
	// to /token#x and fail every exchange at runtime. Checked on the RAW
	// string: a bare trailing "#" parses to an EMPTY fragment that a
	// u.Fragment check would miss.
	if strings.Contains(rawURL, "#") {
		return fmt.Errorf("exchange endpoint URL must not contain a fragment (%q): fragments are never "+
			"sent on the wire, but the private_key_jwt audience uses the URL verbatim and would mismatch", "#")
	}
	return nil
}

// isLoopbackHost reports whether a hostname is proxy-safe loopback: exact
// "localhost" or a loopback IP literal, the two forms Go's proxy resolution
// never routes through HTTP_PROXY. Deliberately not *.localhost, and
// deliberately case-sensitive: the proxy check compares the request's host
// to "localhost" as written, so LOCALHOST would be sent through HTTP_PROXY.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
