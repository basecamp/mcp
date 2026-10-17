package exchange

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basecamp/mcp/auth/upstream/upstreamtest"
)

// TestHostileEndpoint_NothingReachesErrorText runs every exchange against
// every hostile answer in upstreamtest, each reflecting the subject token,
// the configured client credential and a token-shaped value the request
// never carried, and asserts none of them reaches the error's text or a JSON
// log line of it. (basecamp-mcp-server's copy also checks the Sentry event;
// here TestInfraError_HasNoUnwrap pins what keeps Sentry's chain walk off the
// cause.)
func TestHostileEndpoint_NothingReachesErrorText(t *testing.T) {
	const subject = "bc_at_InboundSubjectToken_a1b2c3d4e5f6g7h8" // #nosec G101 -- test fixture
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	clients := []struct {
		name    string
		secrets []string
		auth    func(tokenURL string) ClientAuthenticator
	}{
		{"client_secret_post", []string{"cs_ConfiguredClientSecret_Q7w8e9r0t1y2u3"},
			func(string) ClientAuthenticator {
				return ClientSecretPost("mcp", "cs_ConfiguredClientSecret_Q7w8e9r0t1y2u3")
			}},
		// A numeric secret is what a JSON number field can echo whole.
		{"numeric client_secret", []string{"120938475610293847561029384756"},
			func(string) ClientAuthenticator { return ClientSecretPost("mcp", "120938475610293847561029384756") }},
		// The assertion is minted per request; its JWT header is the same
		// every time, so that is what the test can plant.
		{"private_key_jwt", nil, func(tokenURL string) ClientAuthenticator {
			a, err := PrivateKeyJWT("mcp", key, tokenURL)
			if err != nil {
				t.Fatal(err)
			}
			return a
		}},
	}

	for _, c := range clients {
		for _, kind := range upstreamtest.Kinds {
			t.Run(c.name+"/"+string(kind), func(t *testing.T) {
				var assertion string
				srv := httptest.NewServer(upstreamtest.Handler(kind))
				defer srv.Close()
				auth := c.auth(srv.URL)
				if c.name == "private_key_jwt" {
					form := map[string][]string{}
					if err := auth.Authenticate(form); err != nil {
						t.Fatal(err)
					}
					assertion = form["client_assertion"][0]
				}

				ex, err := New(Config{TokenURL: srv.URL, Auth: auth})
				if err != nil {
					t.Fatal(err)
				}
				_, err = ex.Exchange(context.Background(), subject)
				if err == nil {
					t.Fatal("a hostile answer produced a token")
				}
				if !IsInfra(err) && !IsTerminal(err) {
					t.Fatalf("error is outside the taxonomy: %T", err)
				}

				secrets := append([]string{subject, upstreamtest.Planted, upstreamtest.PlantedNumber}, c.secrets...)
				if assertion != "" {
					secrets = append(secrets, assertion)
				}

				upstreamtest.AssertClean(t, "Error()", err.Error(), secrets...)

				var logs bytes.Buffer
				slog.New(slog.NewJSONHandler(&logs, nil)).Error("authorization upstream unavailable", "error", err)
				upstreamtest.AssertClean(t, "log line", logs.String(), secrets...)
			})
		}
	}
}

// TestInfraError_HasNoUnwrap: Sentry's Go SDK records every link of an
// Unwrap or Cause chain as its own exception, with that link's Error text,
// so either method on InfraError would ship the cause's text. errors.Is and
// errors.As reach the cause through Failure's Is and As instead.
func TestInfraError_HasNoUnwrap(t *testing.T) {
	var e any = newInfra(0, "", "token endpoint request failed", context.DeadlineExceeded)
	if _, ok := e.(interface{ Unwrap() error }); ok {
		t.Fatal("InfraError has Unwrap() error")
	}
	if _, ok := e.(interface{ Unwrap() []error }); ok {
		t.Fatal("InfraError has Unwrap() []error")
	}
	if _, ok := e.(interface{ Cause() error }); ok {
		t.Fatal("InfraError has Cause() error")
	}
}

// TestPartialEchoIsRedacted: an endpoint that echoes only part of a
// credential (a prefix cut at a length limit, a suffix, a middle) leaks it
// as surely as one echoing all of it, and ScrubDescription's token-run
// backstop lets a short piece through. No WindowBytes-long piece of the
// subject token or the client secret reaches the error text or a log line,
// and the rest of the description survives.
func TestPartialEchoIsRedacted(t *testing.T) {
	const subject = "bc_at_1234567890abcdef"                  // #nosec G101 -- test fixture
	const secret = "cs_ConfiguredClientSecret_Q7w8e9r0t1y2u3" // #nosec G101 -- test fixture
	cuts := map[string]string{
		"subject prefix": subject[:16],
		"subject suffix": subject[len(subject)-14:],
		"secret prefix":  secret[:12],
		"secret middle":  secret[10:24],
		"secret suffix":  secret[len(secret)-13:],
	}
	for name, piece := range cuts {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, `{"error":"invalid_client","error_description":"bad token %s here"}`, piece)
			}))
			defer srv.Close()
			ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("mcp", secret)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ex.Exchange(context.Background(), subject)
			if !IsInfra(err) {
				t.Fatalf("want InfraError, got %v", err)
			}
			upstreamtest.AssertClean(t, "Error()", err.Error(), subject, secret)
			var logs bytes.Buffer
			slog.New(slog.NewJSONHandler(&logs, nil)).Error("authorization upstream unavailable", "error", err)
			upstreamtest.AssertClean(t, "log line", logs.String(), subject, secret)
			if !strings.Contains(err.Error(), "bad token [redacted] here") {
				t.Errorf("the rest of the description should survive: %q", err.Error())
			}
		})
	}
}
