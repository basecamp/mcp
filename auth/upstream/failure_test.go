package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
	if got, want := f.Error(), "token exchange infrastructure failure: token endpoint request failed"; got != want {
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
		{"letters beyond ASCII kept", "café ünïcode", "café ünïcode"},
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

func TestScrubDescription_BoundKeepsUTF8Valid(t *testing.T) {
	got := ScrubDescription(strings.Repeat("é", MaxDescriptionBytes))
	if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Fatalf("bound cut a rune: %q", got)
	}
}
