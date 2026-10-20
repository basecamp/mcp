package exchange

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basecamp/mcp/auth/upstream"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const testSubjectToken = "bc_at_subject_token_value"

// stubSigner records Proof/ProofWithNonce calls and returns
// distinguishable proofs.
type stubSigner struct {
	mu    sync.Mutex
	calls []struct{ method, htu, accessToken, nonce string }
}

func (s *stubSigner) record(method, htu, accessToken, nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, struct{ method, htu, accessToken, nonce string }{method, htu, accessToken, nonce})
}

func (s *stubSigner) Proof(method, htu, accessToken string) (string, error) {
	s.record(method, htu, accessToken, "")
	return "stub-dpop-proof", nil
}

func (s *stubSigner) ProofWithNonce(method, htu, accessToken, nonce string) (string, error) {
	s.record(method, htu, accessToken, nonce)
	return "stub-dpop-proof-nonce=" + nonce, nil
}

func (s *stubSigner) Thumbprint() string { return "stub-thumbprint" }

func (s *stubSigner) snapshot() []struct{ method, htu, accessToken, nonce string } {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]struct{ method, htu, accessToken, nonce string }(nil), s.calls...)
}

func tokenJSON(tokenType string, expiresIn int64) string {
	const issued = `"issued_token_type":"urn:ietf:params:oauth:token-type:access_token"`
	if expiresIn > 0 {
		return fmt.Sprintf(`{"access_token":"exchanged-token","token_type":%q,%s,"expires_in":%d}`, tokenType, issued, expiresIn)
	}
	return fmt.Sprintf(`{"access_token":"exchanged-token","token_type":%q,%s}`, tokenType, issued)
}

func newExchanger(t *testing.T, tokenURL string, opts func(*Config)) *Exchanger {
	t.Helper()
	cfg := Config{
		TokenURL: tokenURL,
		Auth:     ClientSecretPost("client-id", "client-secret"),
	}
	if opts != nil {
		opts(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestExchangeSuccess(t *testing.T) {
	var gotForm url.Values
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		mu.Lock()
		gotForm = r.PostForm
		mu.Unlock()
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, tokenJSON("Bearer", 300))
	}))
	defer ts.Close()

	e := newExchanger(t, ts.URL, nil)
	before := time.Now()
	tok, err := e.Exchange(context.Background(), testSubjectToken)
	after := time.Now()
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	mu.Lock()
	form := gotForm
	mu.Unlock()
	for field, want := range map[string]string{ // #nosec G101 -- RFC 8693 URNs and test fixtures, not credentials
		"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
		"subject_token":      testSubjectToken,
		"subject_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"client_id":          "client-id",
		"client_secret":      "client-secret",
	} {
		if got := form.Get(field); got != want {
			t.Errorf("form[%s] = %q, want %q", field, got, want)
		}
	}
	// Resource is optional and unset here: the param must be absent
	// entirely, not sent empty.
	if _, present := form["resource"]; present {
		t.Errorf("resource param sent (%q) despite empty Config.Resource", form.Get("resource"))
	}

	if tok.AccessToken != "exchanged-token" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("TokenType = %q", tok.TokenType)
	}
	wantMin := before.Add(300 * time.Second)
	wantMax := after.Add(300 * time.Second)
	if tok.ExpiresAt.Before(wantMin) || tok.ExpiresAt.After(wantMax) {
		t.Errorf("ExpiresAt = %v, want within [%v, %v]", tok.ExpiresAt, wantMin, wantMax)
	}
}

func TestExchangeWithResource(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.PostForm.Get("resource"); got != "https://3.basecampapi.com" {
			t.Errorf("resource = %q, want verbatim https://3.basecampapi.com", got)
		}
		_, _ = fmt.Fprint(w, tokenJSON("Bearer", 300))
	}))
	defer ts.Close()

	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Resource = "https://3.basecampapi.com" })
	if _, err := e.Exchange(context.Background(), testSubjectToken); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
}

func TestExchangeNoExpiresIn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, tokenJSON("DPoP", 0))
	}))
	defer ts.Close()

	tok, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero when expires_in absent", tok.ExpiresAt)
	}
	if tok.TokenType != "DPoP" {
		t.Errorf("TokenType = %q", tok.TokenType)
	}
}

func TestExchangeDPoPHeader(t *testing.T) {
	var gotProof string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotProof = r.Header.Get("DPoP")
		mu.Unlock()
		_, _ = fmt.Fprint(w, tokenJSON("DPoP", 300))
	}))
	defer ts.Close()

	signer := &stubSigner{}
	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Signer = signer })
	if _, err := e.Exchange(context.Background(), testSubjectToken); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	mu.Lock()
	proof := gotProof
	mu.Unlock()
	if proof != "stub-dpop-proof" {
		t.Errorf("DPoP header = %q, want the stub's proof", proof)
	}
	calls := signer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Proof called %d times, want 1", len(calls))
	}
	call := calls[0]
	if call.method != "POST" || call.htu != ts.URL || call.accessToken != "" || call.nonce != "" {
		t.Errorf("Proof(%q, %q, %q) nonce=%q, want (POST, %s, empty ath, no nonce)", call.method, call.htu, call.accessToken, call.nonce, ts.URL)
	}
}

func TestExchangeNoDPoPHeaderWithoutSigner(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proof := r.Header.Get("DPoP"); proof != "" {
			t.Errorf("unexpected DPoP header %q", proof)
		}
		_, _ = fmt.Fprint(w, tokenJSON("Bearer", 300))
	}))
	defer ts.Close()

	if _, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
}

// TestDPoPNonceRetry drives the full RFC 9449 §8 dance with private_key_jwt
// client auth: the retry must carry the AS-supplied nonce's proof AND a
// freshly minted client assertion (new jti), never a replay of the first.
func TestDPoPNonceRetry(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var proofs, assertions []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		proofs = append(proofs, r.Header.Get("DPoP"))
		assertions = append(assertions, r.PostForm.Get("client_assertion"))
		first := len(proofs) == 1
		mu.Unlock()
		if first {
			w.Header().Set("DPoP-Nonce", "nonce-123")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"use_dpop_nonce"}`)
			return
		}
		_, _ = fmt.Fprint(w, tokenJSON("DPoP", 300))
	}))
	defer ts.Close()

	auth, err := PrivateKeyJWT("jwt-client", key, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	signer := &stubSigner{}
	e := newExchanger(t, ts.URL, func(cfg *Config) {
		cfg.Auth = auth
		cfg.Signer = signer
	})

	tok, err := e.Exchange(context.Background(), testSubjectToken)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "exchanged-token" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(proofs) != 2 {
		t.Fatalf("got %d requests, want 2 (initial + one nonce retry)", len(proofs))
	}
	if proofs[0] != "stub-dpop-proof" {
		t.Errorf("first proof = %q, want the plain proof", proofs[0])
	}
	if proofs[1] != "stub-dpop-proof-nonce=nonce-123" {
		t.Errorf("retry proof = %q, want the nonce-bearing proof", proofs[1])
	}

	calls := signer.snapshot()
	if len(calls) != 2 || calls[0].nonce != "" || calls[1].nonce != "nonce-123" {
		t.Errorf("signer calls = %+v, want plain then nonce-123", calls)
	}

	// Fresh client auth on the retry: different jti.
	jtis := make([]string, 2)
	for i, assertion := range assertions {
		parsed, err := jwt.ParseSigned(assertion, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatalf("assertion %d: %v", i, err)
		}
		var claims jwt.Claims
		if err := parsed.Claims(&key.PublicKey, &claims); err != nil {
			t.Fatalf("assertion %d verify: %v", i, err)
		}
		jtis[i] = claims.ID
	}
	if jtis[0] == "" || jtis[0] == jtis[1] {
		t.Errorf("retry replayed the client assertion (jti %q == %q); each attempt must mint fresh", jtis[0], jtis[1])
	}
}

func TestDPoPNoncePersistentDemandNeverLoops(t *testing.T) {
	var requests int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("DPoP-Nonce", "nonce-again")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":"use_dpop_nonce"}`)
	}))
	defer ts.Close()

	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Signer = &stubSigner{} })
	_, err := e.Exchange(context.Background(), testSubjectToken)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsInfra(err) || IsTerminal(err) {
		t.Errorf("persistent use_dpop_nonce must be infra, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Errorf("made %d requests, want exactly 2 (one deterministic retry, never a loop)", requests)
	}
}

func TestDPoPNonceDemandWithoutHeaderIsInfra(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"use_dpop_nonce"}`)
	}))
	defer ts.Close()

	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Signer = &stubSigner{} })
	_, err := e.Exchange(context.Background(), testSubjectToken)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsInfra(err) || IsTerminal(err) {
		t.Errorf("use_dpop_nonce without DPoP-Nonce header must be infra, got %v", err)
	}
}

func TestDPoPNonceOnSuccessIgnored(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DPoP-Nonce", "for-next-time")
		_, _ = fmt.Fprint(w, tokenJSON("DPoP", 300))
	}))
	defer ts.Close()

	signer := &stubSigner{}
	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Signer = signer })
	tok, err := e.Exchange(context.Background(), testSubjectToken)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "exchanged-token" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if calls := signer.snapshot(); len(calls) != 1 {
		t.Errorf("signer called %d times; a success-response nonce must not trigger a retry", len(calls))
	}
}

func TestRateLimited(t *testing.T) {
	tests := []struct {
		name           string
		retryAfter     string
		wantRetryAfter time.Duration
	}{
		{"delta-seconds", "60", 60 * time.Second},
		{"absent", "", 0},
		{"HTTP-date ignored", "Wed, 21 Oct 2026 07:28:00 GMT", 0},
		{"negative ignored", "-60", 0},
		{"non-numeric ignored", "soon", 0},
		// Multiplied out unbounded, these overflowed time.Duration into a
		// negative backoff.
		{"past the bound", "86401", upstream.MaxRetryAfter},
		{"first overflowing value", "9223372037", upstream.MaxRetryAfter},
		{"max int64", "9223372036854775807", upstream.MaxRetryAfter},
		{"past int64", "99999999999999999999", upstream.MaxRetryAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer ts.Close()

			_, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
			if err == nil {
				t.Fatal("expected error")
			}
			var infra *InfraError
			if !errors.As(err, &infra) {
				t.Fatalf("error is %T, want *InfraError", err)
			}
			if infra.Status != http.StatusTooManyRequests {
				t.Errorf("Status = %d, want 429", infra.Status)
			}
			if infra.RetryAfter != tt.wantRetryAfter {
				t.Errorf("RetryAfter = %v, want %v", infra.RetryAfter, tt.wantRetryAfter)
			}
			if IsTerminal(err) {
				t.Error("429 must never be terminal")
			}
		})
	}
}

func TestPrivateKeyJWTAssertion(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var assertions []string
	var forms []url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		assertions = append(assertions, r.PostForm.Get("client_assertion"))
		forms = append(forms, r.PostForm)
		mu.Unlock()
		_, _ = fmt.Fprint(w, tokenJSON("Bearer", 300))
	}))
	defer ts.Close()

	auth, err := PrivateKeyJWT("jwt-client", key, ts.URL)
	if err != nil {
		t.Fatalf("PrivateKeyJWT: %v", err)
	}
	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Auth = auth })

	for i := 0; i < 2; i++ {
		if _, err := e.Exchange(context.Background(), testSubjectToken); err != nil {
			t.Fatalf("Exchange %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(assertions) != 2 {
		t.Fatalf("got %d assertions, want 2", len(assertions))
	}

	form := forms[0]
	if got := form.Get("client_id"); got != "jwt-client" {
		t.Errorf("client_id = %q", got)
	}
	if got := form.Get("client_assertion_type"); got != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		t.Errorf("client_assertion_type = %q", got)
	}

	jtis := make([]string, 0, len(assertions))
	for i, assertion := range assertions {
		parsed, err := jwt.ParseSigned(assertion, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatalf("assertion %d does not parse as ES256 JWT: %v", i, err)
		}
		var claims jwt.Claims
		// Claims against the public key verifies the signature.
		if err := parsed.Claims(&key.PublicKey, &claims); err != nil {
			t.Fatalf("assertion %d signature verification failed: %v", i, err)
		}
		if claims.Issuer != "jwt-client" || claims.Subject != "jwt-client" {
			t.Errorf("assertion %d: iss=%q sub=%q, want both jwt-client", i, claims.Issuer, claims.Subject)
		}
		if !claims.Audience.Contains(ts.URL) {
			t.Errorf("assertion %d: aud=%v does not contain %s", i, claims.Audience, ts.URL)
		}
		if claims.IssuedAt == nil || claims.Expiry == nil || !claims.Expiry.Time().After(claims.IssuedAt.Time()) {
			t.Errorf("assertion %d: exp must be after iat (iat=%v exp=%v)", i, claims.IssuedAt, claims.Expiry)
		}
		if claims.ID == "" {
			t.Errorf("assertion %d: jti is empty", i)
		}
		jtis = append(jtis, claims.ID)
	}
	if jtis[0] == jtis[1] {
		t.Errorf("consecutive assertions share jti %q; each must be fresh", jtis[0])
	}
}

func TestPrivateKeyJWTRejectsWrongKeys(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := PrivateKeyJWT("c", p384, "https://as.example.com/token"); err == nil {
		t.Error("PrivateKeyJWT accepted a P-384 key")
	}
	if _, err := PrivateKeyJWT("c", ed, "https://as.example.com/token"); err == nil {
		t.Error("PrivateKeyJWT accepted an Ed25519 key")
	}
}

func TestLoadPrivateKeyJWTFromPEM(t *testing.T) {
	dir := t.TempDir()

	writePEM := func(t *testing.T, name, blockType string, der []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalECPrivateKey(p256)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(p256)
	if err != nil {
		t.Fatal(err)
	}
	p384DER, err := x509.MarshalECPrivateKey(p384)
	if err != nil {
		t.Fatal(err)
	}
	garbagePath := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbagePath, []byte("not a pem file at all"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"EC PRIVATE KEY P-256", writePEM(t, "ec.pem", "EC PRIVATE KEY", ecDER), false},
		{"PKCS#8 P-256", writePEM(t, "pkcs8.pem", "PRIVATE KEY", pkcs8DER), false},
		{"P-384 rejected", writePEM(t, "p384.pem", "EC PRIVATE KEY", p384DER), true},
		{"garbage file", garbagePath, true},
		{"missing file", filepath.Join(dir, "nope.pem"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, err := LoadPrivateKeyJWTFromPEM("c", tt.path, "https://as.example.com/token")
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadPrivateKeyJWTFromPEM: %v", err)
			}
			form := url.Values{}
			if err := auth.Authenticate(form); err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if form.Get("client_assertion") == "" {
				t.Error("no client_assertion produced")
			}
		})
	}
}

func TestInvalidGrantIsTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"token not audienced to this resource"}`)
	}))
	defer ts.Close()

	_, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
	if err == nil {
		t.Fatal("expected error")
	}

	var terminal *TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("error is %T, want *TerminalError", err)
	}
	if terminal.Code != "invalid_grant" {
		t.Errorf("Code = %q", terminal.Code)
	}
	if terminal.Description != "token not audienced to this resource" {
		t.Errorf("Description = %q", terminal.Description)
	}
	if !IsTerminal(err) {
		t.Error("IsTerminal = false")
	}
	if IsInfra(err) {
		t.Error("IsInfra = true for a terminal error")
	}
	if strings.Contains(err.Error(), testSubjectToken) {
		t.Error("error string leaks the subject token")
	}
}

func TestInfraErrorResponses(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode string
	}{
		{"invalid_client", http.StatusUnauthorized, `{"error":"invalid_client"}`, "invalid_client"},
		{"invalid_target", http.StatusBadRequest, `{"error":"invalid_target"}`, "invalid_target"},
		// No signer configured, so a use_dpop_nonce demand is endpoint
		// nonsense - generic infra, no retry.
		{"use_dpop_nonce without signer", http.StatusBadRequest, `{"error":"use_dpop_nonce"}`, "use_dpop_nonce"},
		{"unparseable 400", http.StatusBadRequest, `<html>nope</html>`, ""},
		{"other 4xx", http.StatusForbidden, `denied`, ""},
		{"500", http.StatusInternalServerError, `boom`, ""},
		{"200 empty access_token", http.StatusOK, `{"access_token":"","token_type":"Bearer"}`, ""},
		{"200 non-JSON", http.StatusOK, `<html>proxy error page</html>`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer ts.Close()

			_, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
			if err == nil {
				t.Fatal("expected error")
			}
			var infra *InfraError
			if !errors.As(err, &infra) {
				t.Fatalf("error is %T, want *InfraError", err)
			}
			if infra.Status != tt.status {
				t.Errorf("Status = %d, want %d", infra.Status, tt.status)
			}
			if infra.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", infra.Code, tt.wantCode)
			}
			if !IsInfra(err) {
				t.Error("IsInfra = false")
			}
			if IsTerminal(err) {
				t.Error("IsTerminal = true for an infra error")
			}
			if strings.Contains(err.Error(), testSubjectToken) {
				t.Error("error string leaks the subject token")
			}
		})
	}
}

// TestInvalidTargetIsInfraNotTerminal pins the classification bc3 relies
// on: invalid_target is their answer to resource-indicator problems, which
// is OUR misconfiguration - clients must get 503 (retryable), never a
// re-consent 401.
func TestInvalidTargetIsInfraNotTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_target","error_description":"unknown resource"}`)
	}))
	defer ts.Close()

	e := newExchanger(t, ts.URL, func(cfg *Config) { cfg.Resource = "https://wrong.example.com" })
	_, err := e.Exchange(context.Background(), testSubjectToken)
	if err == nil {
		t.Fatal("expected error")
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("error is %T, want *InfraError", err)
	}
	if infra.Code != "invalid_target" {
		t.Errorf("Code = %q", infra.Code)
	}
	if IsTerminal(err) {
		t.Error("invalid_target must never signal re-authorization")
	}
}

// TestSuccessWithExtraFields pins that RFC 8693 response fields beyond the
// three we consume (scope, resource, issued_token_type) do not break
// parsing.
func TestSuccessWithExtraFields(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{
			"access_token": "exchanged-token",
			"token_type": "DPoP",
			"expires_in": 300,
			"scope": "read write",
			"resource": "https://3.basecampapi.com",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token"
		}`)
	}))
	defer ts.Close()

	tok, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "exchanged-token" || tok.TokenType != "DPoP" {
		t.Errorf("Token = %+v", tok)
	}
}

func TestConnectionRefusedIsInfra(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	tokenURL := ts.URL
	ts.Close() // nothing listening any more

	_, err := newExchanger(t, tokenURL, nil).Exchange(context.Background(), testSubjectToken)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsInfra(err) {
		t.Errorf("IsInfra = false for connection refused: %v", err)
	}
	if IsTerminal(err) {
		t.Error("IsTerminal = true for connection refused")
	}
}

func TestContextTimeoutIsInfra(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); ts.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := newExchanger(t, ts.URL, nil).Exchange(ctx, testSubjectToken)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsInfra(err) {
		t.Errorf("IsInfra = false for timeout: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not unwrap to context.DeadlineExceeded: %v", err)
	}
}

func TestOversizedResponseBounded(t *testing.T) {
	// A 200 whose body exceeds the read cap must fail as a malformed
	// response after a bounded read, not stream indefinitely.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"access_token":%q}`, strings.Repeat("a", 256<<10))
	}))
	defer ts.Close()

	_, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
	if err == nil {
		t.Fatal("expected error for oversized response")
	}
	if !IsInfra(err) {
		t.Errorf("IsInfra = false: %v", err)
	}
}

// TestResponsePastTheCapIsInfra: a body longer than MaxResponseBytes is an
// infrastructure failure even when its first MaxResponseBytes bytes hold a
// complete answer, padded out, followed by more: an invalid_grant there
// would force re-authorization, and a token would be served from a
// response that never ended where it claimed to.
func TestResponsePastTheCapIsInfra(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		answer string
	}{
		"invalid_grant": {http.StatusBadRequest, `{"error":"invalid_grant"}`},
		"token":         {http.StatusOK, tokenJSON("Bearer", 300)},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.answer+strings.Repeat(" ", MaxResponseBytes-len(tc.answer))+"trailing")
			}))
			defer ts.Close()
			_, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
			if !IsInfra(err) {
				t.Fatalf("want InfraError, got %v", err)
			}
		})
	}
	// A body exactly at the cap is still read and answered.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := tokenJSON("Bearer", 300)
		_, _ = fmt.Fprint(w, answer+strings.Repeat(" ", MaxResponseBytes-len(answer)))
	}))
	defer ts.Close()
	if _, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken); err != nil {
		t.Fatalf("a body at the cap: %v", err)
	}
}

// TestNonceRetrySharesTheTimeoutUnderALaterDeadline: a caller deadline later
// than the client's timeout doesn't lift the whole-exchange bound.
func TestNonceRetrySharesTheTimeoutUnderALaterDeadline(t *testing.T) {
	const timeout = 400 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(timeout * 3 / 4)
		if strings.Contains(r.Header.Get("DPoP"), "nonce=") {
			_, _ = fmt.Fprint(w, tokenJSON("DPoP", 300))
			return
		}
		w.Header().Set("DPoP-Nonce", "nonce-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"use_dpop_nonce"}`)
	}))
	defer ts.Close()
	ex, err := New(Config{
		TokenURL:   ts.URL,
		Auth:       ClientSecretPost("id", "secret"),
		HTTPClient: &http.Client{Timeout: timeout},
		Signer:     &stubSigner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*timeout)
	defer cancel()
	start := time.Now()
	_, err = ex.Exchange(ctx, testSubjectToken)
	if elapsed := time.Since(start); elapsed > timeout+timeout/2 {
		t.Fatalf("exchange took %v, past one %v timeout", elapsed, timeout)
	}
	if !IsInfra(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an InfraError reaching DeadlineExceeded, got %v", err)
	}
}

func TestNewFailsClosed(t *testing.T) {
	valid := Config{ // #nosec G101 -- test fixture, not a credential
		TokenURL: "https://as.example.com/token",
		Auth:     ClientSecretPost("id", "secret"),
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing TokenURL", func(c *Config) { c.TokenURL = "" }},
		{"non-http TokenURL", func(c *Config) { c.TokenURL = "ftp://as.example.com/token" }},
		{"unparseable TokenURL", func(c *Config) { c.TokenURL = "://bad" }},
		{"missing Auth", func(c *Config) { c.Auth = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Error("New accepted an invalid config")
			}
		})
	}

	// Resource is optional: an empty value must not fail construction.
	if _, err := New(valid); err != nil {
		t.Errorf("New rejected a valid config without Resource: %v", err)
	}
}

// TestSubjectTokenBoundExtension: bc3's extension member reporting whether
// the subject token was DPoP-bound — nil when omitted, so pre-contract
// endpoints are indistinguishable from "unknown", never from "false".
func TestSubjectTokenBoundExtension(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want *bool
	}{
		{"absent means unknown", `{"access_token":"x","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`, nil},
		{"true", `{"access_token":"x","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","subject_token_bound":true}`, boolPtr(true)},
		{"false", `{"access_token":"x","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","subject_token_bound":false}`, boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", "secret")})
			if err != nil {
				t.Fatal(err)
			}
			tok, err := ex.Exchange(context.Background(), "subject")
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil:
				if tok.SubjectTokenBound != nil {
					t.Errorf("SubjectTokenBound = %v, want nil", *tok.SubjectTokenBound)
				}
			case tok.SubjectTokenBound == nil:
				t.Errorf("SubjectTokenBound = nil, want %v", *tc.want)
			case *tok.SubjectTokenBound != *tc.want:
				t.Errorf("SubjectTokenBound = %v, want %v", *tok.SubjectTokenBound, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// TestScopeReported: the issued token's scope is passed through as the AS
// wrote it, and an omitted one is empty, so a caller can tell "unknown" from
// any scope the AS named. An explicit null reads as omitted: both say the AS
// named no scope, and empty is the narrowest reading a caller can take, so
// refusing the token over it would only turn a usable answer into an outage.
// A scope of any other non-string type is a malformed response,
// infrastructure like every other parse failure.
func TestScopeReported(t *testing.T) {
	const base = `"access_token":"x","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"`
	for _, tc := range []struct {
		name string
		body string
		want string
		err  bool
	}{
		{"absent means unknown", `{` + base + `}`, "", false},
		{"null means unknown", `{` + base + `,"scope":null}`, "", false},
		{"one scope", `{` + base + `,"scope":"read"}`, "read", false},
		{"space-delimited, untouched", `{` + base + `,"scope":"read write"}`, "read write", false},
		{"not a string", `{` + base + `,"scope":["read"]}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", "secret")})
			if err != nil {
				t.Fatal(err)
			}
			tok, err := ex.Exchange(context.Background(), "subject")
			switch {
			case tc.err:
				if err == nil || IsTerminal(err) {
					t.Fatalf("Exchange error = %v, want a non-terminal (infrastructure) error", err)
				}
			case err != nil:
				t.Fatal(err)
			case tok.Scope != tc.want:
				t.Errorf("Scope = %q, want %q", tok.Scope, tc.want)
			}
		})
	}
}

// TestRedirectRefused: a token endpoint answering 3xx must become an
// InfraError, never a followed redirect — 307/308 would replay the subject
// token and client credentials to whatever origin Location names.
func TestRedirectRefused(t *testing.T) {
	var leaked int
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked++
	}))
	defer leak.Close()

	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leak.URL, http.StatusTemporaryRedirect)
	}))
	defer as.Close()

	ex, err := New(Config{TokenURL: as.URL, Auth: ClientSecretPost("id", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Exchange(context.Background(), "subject")
	if !IsInfra(err) {
		t.Fatalf("redirect must be InfraError, got %v", err)
	}
	if leaked != 0 {
		t.Fatalf("the redirect target received %d request(s); credentials were replayed cross-origin", leaked)
	}
}

// TestExchangeWithBinding_SendsJKT: the subject_token_dpop_jkt param is sent
// exactly when a jkt is supplied, and never otherwise.
func TestExchangeWithBinding_SendsJKT(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		v, present := r.PostForm["subject_token_dpop_jkt"]
		if present {
			got = append(got, v[0])
		} else {
			got = append(got, "<absent>")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"x","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`)
	}))
	defer srv.Close()

	ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExchangeWithBinding(context.Background(), "subject", "thumb123"); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exchange(context.Background(), "subject"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "thumb123" || got[1] != "<absent>" {
		t.Fatalf("subject_token_dpop_jkt observations = %v, want [thumb123 <absent>]", got)
	}
}

// TestTokenResponseProfileEnforced: RFC 8693 §2.2.1 — a response whose
// issued_token_type is not the access-token URN (or missing), or whose
// token_type names a presentation scheme this server doesn't implement,
// must never be forwarded as an API credential.
func TestTokenResponseProfileEnforced(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing issued_token_type", `{"access_token":"x","token_type":"Bearer"}`},
		{"refresh-token URN", `{"access_token":"x","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:refresh_token"}`},
		{"unsupported token_type", `{"access_token":"x","token_type":"MAC","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`},
		{"missing token_type", `{"access_token":"x","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", "secret")})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ex.Exchange(context.Background(), "subject")
			if !IsInfra(err) {
				t.Fatalf("mis-profiled response must be InfraError, got %v", err)
			}
		})
	}
}

// TestOAuthErrorCodeSanitized: the error member is endpoint-controlled text;
// only the registry SHAPE (short snake_case ASCII) passes through — a token
// echo in the code member collapses to a placeholder before reaching any
// error string, log, or transcript.
func TestOAuthErrorCodeSanitized(t *testing.T) {
	leak := "bc_at_leaked-Token.value" // #nosec G101 -- test fixture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, leak)
	}))
	defer srv.Close()

	ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Exchange(context.Background(), "subject")
	if !IsInfra(err) {
		t.Fatalf("want InfraError, got %v", err)
	}
	if strings.Contains(err.Error(), leak) {
		t.Fatalf("endpoint-controlled error member leaked into the error string: %v", err)
	}
	var ie *InfraError
	if !errors.As(err, &ie) || ie.Code != "unrecognized_error" {
		t.Fatalf("Code = %q, want unrecognized_error", ie.Code)
	}

	// A KNOWN code passes through; an unknown-but-code-shaped string — the
	// exact shape of a bc_at_* token — does not. The shape check tried
	// first admitted credentials; the enumerated list cannot.
	if got := sanitizeOAuthCode("invalid_target"); got != "invalid_target" {
		t.Fatalf("known code mangled: %q", got)
	}
	if got := sanitizeOAuthCode("bc_at_subject_token_value"); got != "unrecognized_error" {
		t.Fatalf("token-shaped code passed the allowlist: %q", got)
	}

	// The RFC 7591 registration codes are enumerated too — the stdio DCR path
	// funnels through this same allowlist.
	for _, code := range []string{
		"invalid_redirect_uri", "invalid_client_metadata",
		"invalid_software_statement", "unapproved_software_statement",
	} {
		if got := sanitizeOAuthCode(code); got != code {
			t.Fatalf("RFC 7591 code mangled: %q -> %q", code, got)
		}
	}
}

// TestErrorDescriptionRedactsAllSubmittedCredentials: a request-echoing
// endpoint must not place the client secret (or any submitted credential)
// into the retained description — redaction happens at this boundary, where
// every form value is known.
func TestErrorDescriptionRedactsAllSubmittedCredentials(t *testing.T) {
	secret := "client-secret-echoed-value" // #nosec G101 -- test fixture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// #nosec G705 -- a deliberately request-echoing FAKE endpoint: the
		// echo is the attack under test, and the client's boundary
		// redaction is the assertion.
		_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"request was: secret=%s subject=%s"}`,
			r.PostForm.Get("client_secret"), r.PostForm.Get("subject_token"))
	}))
	defer srv.Close()

	ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", secret)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Exchange(context.Background(), "subject-token-echoed")
	var te *TerminalError
	if !errors.As(err, &te) {
		t.Fatalf("want TerminalError, got %v", err)
	}
	if strings.Contains(te.Description, secret) || strings.Contains(te.Description, "subject-token-echoed") {
		t.Fatalf("submitted credential survived boundary redaction: %q", te.Description)
	}
	if !strings.Contains(te.Description, "[redacted]") {
		t.Fatalf("expected redaction markers, got %q", te.Description)
	}
}

// TestErrorDescriptionRedactsControlSplitCredentials: a credential split by
// control characters must not dodge boundary redaction and reassemble
// downstream — controls are stripped first, then values redacted.
func TestErrorDescriptionRedactsControlSplitCredentials(t *testing.T) {
	secret := "client-secret-split-echo" // #nosec G101 -- test fixture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// Echo the secret split by an (escaped) newline.
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"was: client-secret-\nsplit-echo"}`)
	}))
	defer srv.Close()

	ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", secret)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Exchange(context.Background(), "subject-tok")
	var te *TerminalError
	if !errors.As(err, &te) {
		t.Fatalf("want TerminalError, got %v", err)
	}
	if strings.Contains(te.Description, secret) {
		t.Fatalf("control-split credential reassembled into the description: %q", te.Description)
	}
}

// TestErrorsCarryNoEndpointText: what a token endpoint returns can echo a
// credential, so no error string carries it — a redirect whose Location
// fails to parse, a decode error quoting an overflowing number, an
// out-of-range lifetime — and the transport cause stays reachable.
func TestErrorsCarryNoEndpointText(t *testing.T) {
	const secret = "123456789012345678901234567890"
	issued := `"issued_token_type":"urn:ietf:params:oauth:token-type:access_token"`
	cases := map[string]http.HandlerFunc{
		"malformed redirect": func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Location"] = []string{"/" + secret + "%zz"}
			w.WriteHeader(http.StatusTemporaryRedirect)
		},
		"overflowing expires_in": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"access_token":"x","token_type":"Bearer",%s,"expires_in":%s}`, issued, secret)
		},
		"nonpositive expires_in": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"access_token":"x","token_type":"Bearer",%s,"expires_in":-%s}`, issued, secret[:18])
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(handler)
			defer ts.Close()
			tok, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
			if !IsInfra(err) {
				t.Fatalf("want InfraError, got %v (token %+v)", err, tok)
			}
			for _, leak := range []string{secret, secret[:18]} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error text carries endpoint-controlled %q: %v", leak, err)
				}
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	_, err := newExchanger(t, ts.URL, nil).Exchange(ctx, testSubjectToken)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the transport cause must stay reachable, got %v", err)
	}
}

// TestRefusesAnUnprintableAccessToken: a token that can't go in an
// Authorization header is an exchange failure, not a cached credential.
func TestRefusesAnUnprintableAccessToken(t *testing.T) {
	for _, tok := range []string{"a b", "a\r\nX-Injected: 1", "a\u0000b", "caf\u00e9"} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"access_token":"%s","token_type":"Bearer","issued_token_type":"urn:ietf:params:oauth:token-type:access_token"}`, tok)
		}))
		_, err := newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
		ts.Close()
		if !IsInfra(err) {
			t.Errorf("%q: want InfraError, got %v", tok, err)
		}
	}
}

// TestTokenURLRequiresTLSOffLoopback: the exchange sends credentials, so
// only exact localhost or a loopback IP may take plain http; userinfo and a
// fragment are refused, and no error renders a credential in the URL.
func TestTokenURLRequiresTLSOffLoopback(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://fizzy.example/oauth/token":         true,
		"http://127.0.0.1:3000/oauth/token":         true,
		"http://[::1]:3000/oauth/token":             true,
		"http://localhost:3000/oauth/token":         true,
		"http://LOCALHOST:3000/oauth/token":         false, // HTTP_PROXY would get it
		"http://fizzy.example/oauth/token":          false,
		"http://10.0.0.1/oauth/token":               false,
		"http://as.localhost/oauth/token":           false,
		"ftp://fizzy.example/oauth/token":           false,
		"https://:443/oauth/token":                  false,
		"https://user:pw-secret@fizzy.example/t":    false,
		"https://tok-secret@fizzy.example/t":        false,
		"https://fizzy.example/oauth/token#x":       false,
		"https://fizzy.example/oauth/token#":        false,
		"http://fizzy.example/token?api_key=secret": false,
		"http://user:pw-secret@fizzy.example/token": false,
	} {
		_, err := New(Config{TokenURL: raw, Auth: ClientSecretPost("id", "secret")})
		if (err == nil) != ok {
			t.Errorf("New(%q): err = %v, want ok=%v", raw, err, ok)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("New(%q): error renders the URL's credential: %v", raw, err)
		}
	}
}

// TestExpiresInBounds: expires_in is endpoint-controlled. A huge value would
// overflow time.Duration into a token that is already expired; it takes
// upstream.MaxLifetime instead. Values that are not a positive integer stay
// infrastructure failures.
func TestExpiresInBounds(t *testing.T) {
	serve := func(expiresIn string) (*Token, error) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"access_token":"exchanged-token","token_type":"Bearer",`+
				`"issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":%s}`, expiresIn)
		}))
		defer ts.Close()
		return newExchanger(t, ts.URL, nil).Exchange(context.Background(), testSubjectToken)
	}

	for _, v := range []string{"9223372036", "9223372037", "9223372036854775807"} {
		before := time.Now()
		tok, err := serve(v)
		if err != nil {
			t.Fatalf("expires_in %s: %v", v, err)
		}
		if !tok.ExpiresAt.After(before) {
			t.Errorf("expires_in %s: ExpiresAt %v is not in the future", v, tok.ExpiresAt)
		}
		if tok.ExpiresAt.After(time.Now().Add(upstream.MaxLifetime)) {
			t.Errorf("expires_in %s: ExpiresAt %v is past the bound", v, tok.ExpiresAt)
		}
	}

	for _, v := range []string{"0", "-1", "-9223372036854775808", "99999999999999999999", "300.5", `"300"`, "null"} {
		tok, err := serve(v)
		if v == "null" {
			// null is an absent lifetime: unknown, cached under the TTL cap.
			if err != nil || !tok.ExpiresAt.IsZero() {
				t.Errorf("expires_in null: tok=%+v err=%v, want a token with zero ExpiresAt", tok, err)
			}
			continue
		}
		if !IsInfra(err) {
			t.Errorf("expires_in %s: want InfraError, got tok=%+v err=%v", v, tok, err)
		}
	}
}

// TestNonceRetrySharesTheTimeout: with no caller deadline, the client's
// timeout bounds the whole exchange, nonce retry included, not each attempt.
func TestNonceRetrySharesTheTimeout(t *testing.T) {
	const timeout = 400 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(timeout * 3 / 4)
		if strings.Contains(r.Header.Get("DPoP"), "nonce=") {
			_, _ = fmt.Fprint(w, tokenJSON("DPoP", 300))
			return
		}
		w.Header().Set("DPoP-Nonce", "nonce-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"use_dpop_nonce"}`)
	}))
	defer ts.Close()

	ex, err := New(Config{
		TokenURL:   ts.URL,
		Auth:       ClientSecretPost("id", "secret"),
		HTTPClient: &http.Client{Timeout: timeout},
		Signer:     &stubSigner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = ex.Exchange(context.Background(), testSubjectToken)
	if elapsed := time.Since(start); elapsed > timeout+timeout/2 {
		t.Fatalf("exchange took %v, past one %v timeout", elapsed, timeout)
	}
	if !IsInfra(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an InfraError reaching DeadlineExceeded, got %v", err)
	}
}

// TestPrivateKeyJWTRefusesAnUninitializedKey: a typed-nil, zero, partly
// initialized or inconsistent key is a startup error, not a panic on the
// first exchange.
func TestPrivateKeyJWTRefusesAnUninitializedKey(t *testing.T) {
	var typedNil *ecdsa.PrivateKey
	good, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mismatched := &ecdsa.PrivateKey{PublicKey: other.PublicKey, D: good.D}
	for name, key := range map[string]*ecdsa.PrivateKey{
		"typed nil":        typedNil,
		"zero":             {},
		"curve only":       {PublicKey: ecdsa.PublicKey{Curve: elliptic.P256()}},
		"no scalar":        {PublicKey: good.PublicKey},
		"mismatched point": mismatched,
	} {
		if _, err := PrivateKeyJWT("id", key, "https://fizzy.example/oauth/token"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := PrivateKeyJWT("id", good, "https://fizzy.example/oauth/token"); err != nil {
		t.Errorf("a valid key: %v", err)
	}
}

// TestPrivateKeyJWTCurveIsComparedNotCalled: a key whose Curve is some
// other implementation, a typed nil included, is refused without a call
// into it.
func TestPrivateKeyJWTCurveIsComparedNotCalled(t *testing.T) {
	good, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var nilCurve *nilParamsCurve
	key := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: nilCurve, X: good.X, Y: good.Y}, D: good.D}
	if _, err := PrivateKeyJWT("id", key, "https://fizzy.example/oauth/token"); err == nil {
		t.Fatal("want an error for a non-P-256 curve")
	}
}

// nilParamsCurve panics on any call through a nil receiver.
type nilParamsCurve struct{ elliptic.Curve }

// TestNewRefusesAMismatchedAssertionAudience: a private_key_jwt
// authenticator built for one URL and an exchanger posting to another would
// fail every exchange, so New refuses the pair.
func TestNewRefusesAMismatchedAssertionAudience(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := PrivateKeyJWT("id", key, "https://as.example.com/token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{TokenURL: "https://as.example.com/other", Auth: auth}); err == nil {
		t.Error("want an error for a mismatched audience")
	}
	if _, err := New(Config{TokenURL: "https://as.example.com/token", Auth: auth}); err != nil {
		t.Errorf("a matching URL: %v", err)
	}
}

// TestDroppedRuneCannotRejoinASecret: a credential split by any rune the
// rendered text drops (U+FFFD, NEL, a line separator, a zero-width space)
// must be redacted, not rejoined when the description is rendered.
func TestDroppedRuneCannotRejoinASecret(t *testing.T) {
	const secret = "abcdef123456789" // #nosec G101 -- test fixture
	for _, split := range []string{`\ufffd`, `\u0085`, `\u2028`, `\u200b`} {
		t.Run(split, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, `{"error":"invalid_client","error_description":"was abcdef123%s456789"}`, split)
			}))
			defer srv.Close()
			ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", secret)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ex.Exchange(context.Background(), "subject-tok")
			if !IsInfra(err) {
				t.Fatalf("want InfraError, got %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("secret rejoined in the error text: %q", err.Error())
			}
		})
	}
}


// TestErrorDescriptionRedactsReplacementSplitCredentials: a credential split
// by U+FFFD, or by bytes that are not valid UTF-8 (which the JSON decoder
// turns into U+FFFD), must not dodge exact redaction and then rejoin when the
// rendering drops the non-ASCII between its halves. The secrets are shorter
// than the token-run backstop, so only the exact match can catch them, and
// both the terminal description and an infrastructure failure's rendered
// text are checked.
func TestErrorDescriptionRedactsReplacementSplitCredentials(t *testing.T) {
	const (
		secret  = "abcdef123456789" // #nosec G101 -- test fixture
		subject = "subj0123456789"
	)
	splitters := map[string]string{
		"escaped U+FFFD":       `�`,
		"literal U+FFFD":       "�",
		"lone surrogate":       `\ud800`,
		"invalid byte 0xff":    "\xff",
		"overlong NUL":         "\xc0\x80",
		"truncated multi-byte": "\xe2\x82",
		"double U+FFFD":        "��",
	}
	split := func(s, sep string) string { return s[:6] + sep + s[6:] }
	for name, sep := range splitters {
		for _, code := range []string{"invalid_grant", "invalid_client"} {
			t.Run(name+"/"+code, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"error":%q,"error_description":"secret=%s subject=%s"}`,
						code, split(secret, sep), split(subject, sep))
				}))
				defer srv.Close()

				ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", secret)})
				if err != nil {
					t.Fatal(err)
				}
				_, err = ex.Exchange(context.Background(), subject)
				if err == nil {
					t.Fatal("want an error")
				}
				texts := []string{err.Error()}
				var te *TerminalError
				if errors.As(err, &te) {
					texts = append(texts, te.Description, upstream.ScrubDescription(te.Description))
				}
				var ie *InfraError
				if errors.As(err, &ie) {
					texts = append(texts, ie.Description)
				}
				if te == nil && ie == nil {
					t.Fatalf("want TerminalError or InfraError, got %T: %v", err, err)
				}
				for _, text := range texts {
					// Also drop everything but printable ASCII, as a viewer
					// or normalizer downstream might.
					for _, s := range []string{text, upstream.PrintableASCII(text)} {
						if strings.Contains(s, secret) || strings.Contains(s, subject) {
							t.Fatalf("split credential rejoined: %q", s)
						}
					}
				}
			})
		}
	}
}

// TestInvisibleMarkCannotSplitASecret: a combining mark (variation selector
// 16, combining grapheme joiner) is invisible but neither a control nor a
// format character. Threaded through an echoed credential every few bytes,
// it splits the value into pieces shorter than both the fragment and the
// token-run redactions, and a viewer that drops it shows the credential
// whole. Only printable ASCII survives the strip, so the pieces rejoin before
// redaction sees them.
func TestInvisibleMarkCannotSplitASecret(t *testing.T) {
	const secret = "abcdef123456789" // #nosec G101 -- test fixture
	for _, mark := range []string{"͏", "️", "⃝"} {
		t.Run(fmt.Sprintf("%U", []rune(mark)[0]), func(t *testing.T) {
			var threaded strings.Builder
			for i := 0; i < len(secret); i += 3 {
				threaded.WriteString(secret[i:min(i+3, len(secret))] + mark)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, `{"error":"invalid_client","error_description":"was %s"}`, threaded.String())
			}))
			defer srv.Close()
			ex, err := New(Config{TokenURL: srv.URL, Auth: ClientSecretPost("id", secret)})
			if err != nil {
				t.Fatal(err)
			}
			_, err = ex.Exchange(context.Background(), "subject-tok")
			if !IsInfra(err) {
				t.Fatalf("want InfraError, got %v", err)
			}
			// As a viewer that drops what it can't show would render it.
			shown := strings.Map(func(r rune) rune {
				if r < 0x20 || r >= 0x7f {
					return -1
				}
				return r
			}, err.Error())
			if strings.Contains(shown, secret) {
				t.Fatalf("secret rejoined in the error text: %q", err.Error())
			}
		})
	}
}
