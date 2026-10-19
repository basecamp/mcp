package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

const secret = "bc_at_SecretTheCauseQuotes_0123456789abcdef" // #nosec G101 -- test fixture

// causeQuoting is what the standard library builds from response bytes: a
// malformed Location, wrapped the way http.Client.Do wraps it.
func causeQuoting() error {
	return &url.Error{Op: "Post", URL: "https://bc3.example/oauth/tokens",
		Err: fmt.Errorf("failed to parse Location header %q: %w", "/%zz/"+secret, context.DeadlineExceeded)}
}

func TestFailure_TextNeverRendersTheCause(t *testing.T) {
	f := New("token exchange infrastructure failure", "token endpoint request failed", 0, causeQuoting())
	if strings.Contains(f.Error(), secret) || strings.Contains(f.Error(), "Location") {
		t.Fatalf("cause text reached Error(): %q", f.Error())
	}
	// The cause's class, in fixed text, is all of it that renders.
	if got, want := f.Error(), "token exchange infrastructure failure: token endpoint request failed: deadline exceeded"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestFailure_CauseStaysReachableByIsAndAs(t *testing.T) {
	f := New("op", "reason", 0, causeQuoting())
	wrapped := errors.Join(errors.New("authorization upstream unavailable"), f)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Fatal("errors.Is no longer reaches the cause")
	}
	var ue *url.Error
	if !errors.As(wrapped, &ue) {
		t.Fatal("errors.As no longer reaches the cause")
	}
	var got *Failure
	if !errors.As(wrapped, &got) || got != f {
		t.Fatal("errors.As does not find the Failure itself")
	}
	if New("op", "reason", 0, nil).Is(context.DeadlineExceeded) {
		t.Fatal("a Failure without a cause matched a target")
	}
}

// TestFailure_HasNoUnwrap pins the Sentry half of the boundary: the Go SDK
// records every link of an Unwrap or Cause chain as its own exception, with
// that link's Error text, so either method would ship the cause's text.
func TestFailure_HasNoUnwrap(t *testing.T) {
	var f any = New("op", "reason", 0, causeQuoting())
	if _, ok := f.(interface{ Unwrap() error }); ok {
		t.Fatal("Failure has Unwrap() error")
	}
	if _, ok := f.(interface{ Unwrap() []error }); ok {
		t.Fatal("Failure has Unwrap() []error")
	}
	if _, ok := f.(interface{ Cause() error }); ok {
		t.Fatal("Failure has Cause() error")
	}
}

func TestFailure_RendersStatusCodeAndScrubbedDescription(t *testing.T) {
	f := New("token refresh failed", "authorization server rejected the request", 400, nil)
	f.Code = "invalid_grant"
	f.Description = "grant rejected for " + secret
	got := f.Error()
	want := "token refresh failed (status 400) [invalid_grant]: authorization server rejected the request (grant rejected for [redacted])"
	if got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestFailure_UnlistedCodeCollapses(t *testing.T) {
	f := New("op", "reason", 400, nil)
	f.Code = secret
	if strings.Contains(f.Error(), secret) || !strings.Contains(f.Error(), "["+UnrecognizedOAuthCode+"]") {
		t.Fatalf("an unlisted code rendered as itself: %q", f.Error())
	}
}

func TestScrubDescription(t *testing.T) {
	long := strings.Repeat("word ", 100)
	tests := []struct {
		name, in, want string
	}{
		{"plain text survives", "Subject token is not account-bound", "Subject token is not account-bound"},
		{"token-shaped run redacted", "bad token " + secret + " here", "bad token [redacted] here"},
		{"JWT redacted", "assertion eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl rejected", "assertion [redacted] rejected"},
		{"run at the end redacted", "was " + secret, "was [redacted]"},
		{"19 bytes kept", "x 0123456789abcdefghi y", "x 0123456789abcdefghi y"},
		{"20 bytes redacted", "x 0123456789abcdefghij y", "x [redacted] y"},
		{"controls stripped before the run is measured", "x bc_at_0123\n456789abcdef y", "x [redacted] y"},
		{"Unicode line controls stripped", "a\u0085b\u2028c\u2029d", "abcd"},
		{"format characters stripped", "a\u202eb\u200bc", "abc"},
		{"letters beyond ASCII dropped", "caf\u00e9 \u00fcn\u00efcode", "caf ncode"},
		{"U+FFFD and invalid bytes dropped", "a\ufffdb\xffc", "abc"},
		{"bounded", long, strings.TrimSpace(long)[:MaxDescriptionBytes] + "…"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScrubDescription(tt.in); got != tt.want {
				t.Fatalf("ScrubDescription(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestScrubDescription_BoundNeverExposesATokenPrefix: the bound is applied
// after redaction, so a token straddling it is redacted whole rather than
// cut to a prefix shorter than the run length.
func TestScrubDescription_BoundNeverExposesATokenPrefix(t *testing.T) {
	in := strings.Repeat("a ", (MaxDescriptionBytes-10)/2) + secret
	got := ScrubDescription(in)
	if strings.Contains(got, secret[:10]) {
		t.Fatalf("a token prefix survived the bound: %q", got)
	}
	if len(got) > MaxDescriptionBytes+len("…") {
		t.Fatalf("result is %d bytes, over the bound", len(got))
	}
}

// TestScrubDescription_KeepsOnlyPrintableASCII: no invisible character of
// any class (C1, format, combining mark) can split a token into short runs.
func TestScrubDescription_KeepsOnlyPrintableASCII(t *testing.T) {
	in := "caf\u00e9 bc_at_0123\u0085456\u200b789\ufe0fabc\u034fdef\u2060ghij y"
	if got, want := ScrubDescription(in), "caf [redacted] y"; got != want {
		t.Fatalf("ScrubDescription = %q, want %q", got, want)
	}
}

func TestCauseClass(t *testing.T) {
	var num int64
	typeErr := json.Unmarshal([]byte(`{"expires_in":`+"99999999999999999999"+`}`), &struct {
		ExpiresIn *int64 `json:"expires_in"`
	}{&num})
	syntaxErr := json.Unmarshal([]byte(`{"a":`+secret), &num)
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"none", nil, ""},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "deadline exceeded"},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "connection refused"},
		{"other network", &net.OpError{Op: "read", Err: errors.New(secret)}, "network error during read"},
		{"dns", &net.DNSError{Err: "no such host", Name: secret}, "DNS lookup failed"},
		{"eof", fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), "response ended early"},
		{"json type", typeErr, "unexpected JSON value for expires_in"},
		{"json syntax", syntaxErr, ""},
		{"unknown", errors.New(secret), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := causeClass(tt.err)
			if tt.name == "json syntax" {
				// An unterminated value is an unexpected EOF to the decoder.
				if got != "response ended early" && got != "response is not valid JSON" {
					t.Fatalf("causeClass = %q", got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("causeClass = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("causeClass rendered cause text: %q", got)
			}
		})
	}
}
