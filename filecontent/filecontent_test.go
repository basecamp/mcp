package filecontent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	surfguard "github.com/basecamp/surfguard/go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/mcp/filecontent"
)

// fakeFileHost is OpenAI's file host: it serves at any path and counts the
// requests it answers.
type fakeFileHost struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newFakeFileHost(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) *fakeFileHost {
	t.Helper()
	f := &fakeFileHost{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		serve(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// pinTo is a fetcher pinned to srv's host, under the production policy
// opened to loopback and any port so it can reach the test server, which it
// trusts.
func pinTo(t *testing.T, srv *httptest.Server) *filecontent.Fetcher {
	t.Helper()
	return pinWith(t, srv, filecontent.ProductionPolicy().AllowLoopback().AllowAllPorts())
}

func pinWith(t *testing.T, srv *httptest.Server, policy surfguard.Policy) *filecontent.Fetcher {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return filecontent.NewFetcher([]string{u.Host}, policy, roots)
}

// secretPath stands in for the signed part of a real download_url: it must
// appear in no error and no log line.
const secretPath = "/file-abc123/raw?sig=SECRET-SIGNATURE-xyz&se=2026"

var pngBytes = []byte("\x89PNG\r\n\x1a\nnot really a png, but bytes")

func fileObject(downloadURL string) map[string]any {
	return map[string]any{"download_url": downloadURL, "file_id": "file_123", "mime_type": "image/png", "file_name": "notes.png"}
}

func newBudget(t *testing.T) *filecontent.MemoryBudget {
	t.Helper()
	b, err := filecontent.NewMemoryBudget(64 << 20)
	require.NoError(t, err)
	return b
}

// chatGPT is a ChatGPT call's options: file accepted, fetched through
// fetcher, under budget, logging to logs.
func chatGPT(fetcher *filecontent.Fetcher, budget filecontent.Budget, logs *bytes.Buffer) filecontent.Options {
	return filecontent.Options{
		MaxSize:     1 << 20,
		AcceptsFile: true,
		Fetcher:     fetcher,
		Budget:      budget,
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// assertNoURL fails when the download URL's secret part reached the error
// or the logs.
func assertNoURL(t *testing.T, err error, logs *bytes.Buffer) {
	t.Helper()
	text := ""
	if err != nil {
		text = err.Error()
	}
	for _, secret := range []string{"SECRET-SIGNATURE", "file-abc123"} {
		assert.NotContains(t, text, secret, "the error carries the download URL")
		assert.NotContains(t, logs.String(), secret, "a log line carries the download URL")
	}
}

func refusal(t *testing.T, err error) *filecontent.Error {
	t.Helper()
	var fe *filecontent.Error
	require.True(t, errors.As(err, &fe), "a *filecontent.Error: %v", err)
	return fe
}

func TestRead_ChatGPTFileIsFetched(t *testing.T) {
	host := newFakeFileHost(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "no credential is sent to the file host")
		assert.Empty(t, r.Header.Get("Cookie"))
		_, _ = w.Write(pngBytes)
	})
	var logs bytes.Buffer
	sum := sha256.Sum256(pngBytes)
	content, err := filecontent.Read(context.Background(),
		map[string]any{"file": fileObject(host.srv.URL + secretPath), "sha256": hex.EncodeToString(sum[:])},
		chatGPT(pinTo(t, host.srv), newBudget(t), &logs))
	require.NoError(t, err)
	defer content.Done()
	assert.Equal(t, pngBytes, content.Data)
	assert.Equal(t, int64(1), host.hits.Load())
	assertNoURL(t, err, &logs)
}

// Each refusal reads nothing from a host it isn't allowed to reach, and
// names no URL.
func TestRead_ChatGPTFileRefusals(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the download_url to send, the fetcher, and the file
		// host that must see no request (nil: none to check).
		setup     func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost)
		wantText  string
		wantKind  filecontent.Kind
		wantFetch int64
	}{
		{
			name: "a host other than OpenAI's",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
				other := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
				return other.srv.URL + secretPath, pinTo(t, pinned.srv), other
			},
			wantText: "must be on OpenAI's file host",
			wantKind: filecontent.Invalid,
		},
		{
			name: "plain http to the pinned host",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
				return "http://" + strings.TrimPrefix(pinned.srv.URL, "https://") + secretPath, pinTo(t, pinned.srv), pinned
			},
			wantText: "must be on OpenAI's file host",
			wantKind: filecontent.Invalid,
		},
		{
			name: "a redirect off the host",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				other := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, other.srv.URL+secretPath, http.StatusFound)
				})
				return pinned.srv.URL + secretPath, pinTo(t, pinned.srv), other
			},
			wantText:  "redirected away from OpenAI's file host",
			wantKind:  filecontent.Invalid,
			wantFetch: 1,
		},
		{
			name: "an oversize file, streamed with no Content-Length",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) {
					chunk := bytes.Repeat([]byte("x"), 64<<10)
					for range 40 { // 2.5 MiB, past the 1 MiB limit
						if _, err := w.Write(chunk); err != nil {
							return
						}
						if f, ok := w.(http.Flusher); ok {
							f.Flush()
						}
					}
				})
				return pinned.srv.URL + secretPath, pinTo(t, pinned.srv), nil
			},
			wantText: "file too large",
			wantKind: filecontent.TooLarge,
		},
		{
			name: "an oversize file, declared by Content-Length",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Length", "2097152")
					_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20))
				})
				return pinned.srv.URL + secretPath, pinTo(t, pinned.srv), nil
			},
			wantText: "file too large: 2097152 bytes exceeds maximum of 1048576 bytes",
			wantKind: filecontent.TooLarge,
		},
		{
			name: "an expired link",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
				return pinned.srv.URL + secretPath, pinTo(t, pinned.srv), nil
			},
			wantText: "HTTP 403",
			wantKind: filecontent.Invalid,
		},
		{
			name: "the production address policy, refusing the loopback dial",
			setup: func(t *testing.T) (string, *filecontent.Fetcher, *fakeFileHost) {
				pinned := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
				return pinned.srv.URL + secretPath, pinWith(t, pinned.srv, filecontent.ProductionPolicy()), pinned
			},
			wantText: "can't be fetched from this server",
			wantKind: filecontent.Invalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			downloadURL, fetcher, untouched := tc.setup(t)
			budget := newBudget(t)
			var logs bytes.Buffer
			content, err := filecontent.Read(context.Background(),
				map[string]any{"file": fileObject(downloadURL)}, chatGPT(fetcher, budget, &logs))
			require.Error(t, err)
			assert.Nil(t, content)
			fe := refusal(t, err)
			assert.Contains(t, fe.Error(), tc.wantText)
			assert.Equal(t, tc.wantKind, fe.Kind)
			if untouched != nil {
				assert.Zero(t, untouched.hits.Load(), "the host off the pin is never reached")
			}
			assert.Zero(t, budget.Held(), "a refusal holds none of the budget")
			assertNoURL(t, err, &logs)
		})
	}
}

// A ChatGPT call naming file and sending null for content_base64 and
// sha256, as a model may for arguments it isn't using, reads them as absent.
func TestRead_NullOptionalsReadAsAbsent(t *testing.T) {
	host := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
	var logs bytes.Buffer
	content, err := filecontent.Read(context.Background(),
		map[string]any{"file": fileObject(host.srv.URL + secretPath), "content_base64": nil, "sha256": nil},
		chatGPT(pinTo(t, host.srv), newBudget(t), &logs))
	require.NoError(t, err)
	defer content.Done()
	assert.Equal(t, pngBytes, content.Data)
}

// The file argument is ChatGPT's alone: another client's call carrying it is
// refused, fetching nothing.
func TestRead_FileRefusedWhenNotAccepted(t *testing.T) {
	host := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
	var logs bytes.Buffer
	opts := chatGPT(pinTo(t, host.srv), newBudget(t), &logs)
	opts.AcceptsFile = false
	_, err := filecontent.Read(context.Background(), map[string]any{"file": fileObject(host.srv.URL + secretPath)}, opts)
	fe := refusal(t, err)
	assert.Equal(t, filecontent.Invalid, fe.Kind)
	assert.Equal(t, "unknown argument file: send the content as content_base64", fe.Error())
	assert.Zero(t, host.hits.Load())
}

func TestRead_ExactlyOneContentSource(t *testing.T) {
	content := base64.StdEncoding.EncodeToString(pngBytes)
	for _, tc := range []struct {
		name     string
		accepts  bool
		args     map[string]any
		want     string
		wantKind filecontent.Kind
	}{
		{"both, ChatGPT", true, map[string]any{"file": fileObject("https://files.oaiusercontent.com/x"), "content_base64": content}, "send exactly one of file or content_base64, not both", filecontent.Invalid},
		{"neither, ChatGPT", true, map[string]any{}, "send exactly one of file or content_base64", filecontent.Invalid},
		{"neither, another client", false, map[string]any{}, "missing required parameter: content_base64", filecontent.Missing},
		{"a file that isn't an object", true, map[string]any{"file": "chat_upload"}, "file must be the file object", filecontent.Invalid},
		{"a file object with no download_url", true, map[string]any{"file": map[string]any{"file_id": "file_1"}}, "missing required parameter: file.download_url", filecontent.Missing},
		{"file and a non-string content_base64, ChatGPT", true, map[string]any{"file": fileObject("https://files.oaiusercontent.com/x"), "content_base64": float64(123)}, "content_base64 must be a string", filecontent.Invalid},
		{"a non-string content_base64, another client", false, map[string]any{"content_base64": float64(123)}, "content_base64 must be a string", filecontent.Invalid},
		{"null content_base64 and no file, another client", false, map[string]any{"content_base64": nil}, "missing required parameter: content_base64", filecontent.Missing},
		{"null file and null content_base64, ChatGPT", true, map[string]any{"file": nil, "content_base64": nil}, "send exactly one of file or content_base64", filecontent.Invalid},
		{"not base64", false, map[string]any{"content_base64": "not base64!"}, "invalid base64 encoding in content_base64", filecontent.Malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			opts := chatGPT(nil, newBudget(t), &logs)
			opts.AcceptsFile = tc.accepts
			_, err := filecontent.Read(context.Background(), tc.args, opts)
			fe := refusal(t, err)
			assert.Contains(t, fe.Error(), tc.want)
			assert.Equal(t, tc.wantKind, fe.Kind)
		})
	}
}

// sha256 beside content_base64: a match reads, a mismatch refuses, and
// leaving it out reads as before.
func TestRead_SHA256(t *testing.T) {
	content := base64.StdEncoding.EncodeToString(pngBytes)
	sum := sha256.Sum256(pngBytes)
	match := hex.EncodeToString(sum[:])
	altered := sha256.Sum256(append([]byte{0}, pngBytes...))
	for _, tc := range []struct {
		name   string
		sha256 any
		want   string // empty: reads
	}{
		{"match", match, ""},
		{"match, upper case", strings.ToUpper(match), ""},
		{"match, padded", " " + match + "\n", ""},
		{"absent", nil, ""},
		{"null", "null", ""},
		{"mismatch", hex.EncodeToString(altered[:]), "the content was altered in transit: its SHA-256 doesn't match sha256, so nothing was uploaded. Re-encode the file from its source and send it again"},
		{"malformed", "not-a-hash", "sha256 must be the content's SHA-256 as 64 hex digits"},
		{"too short", match[:62], "sha256 must be the content's SHA-256 as 64 hex digits"},
		{"not a string", float64(123), "sha256 must be the content's SHA-256 as 64 hex digits"},
		{"an object", map[string]any{}, "sha256 must be the content's SHA-256 as 64 hex digits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"content_base64": content}
			switch tc.sha256 {
			case nil:
			case "null":
				args["sha256"] = nil
			default:
				args["sha256"] = tc.sha256
			}
			got, err := filecontent.Read(context.Background(), args, filecontent.Options{MaxSize: 1 << 20})
			if tc.want == "" {
				require.NoError(t, err)
				assert.Equal(t, pngBytes, got.Data)
				got.Done()
				return
			}
			fe := refusal(t, err)
			assert.Equal(t, filecontent.Invalid, fe.Kind)
			assert.Equal(t, tc.want, fe.Error())
		})
	}
}

// A malformed sha256 is refused before anything is fetched.
func TestRead_MalformedSHA256FetchesNothing(t *testing.T) {
	host := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
	var logs bytes.Buffer
	_, err := filecontent.Read(context.Background(),
		map[string]any{"file": fileObject(host.srv.URL + secretPath), "sha256": "nope"},
		chatGPT(pinTo(t, host.srv), newBudget(t), &logs))
	require.Error(t, err)
	assert.Zero(t, host.hits.Load())
}

func TestRead_Base64Size(t *testing.T) {
	opts := filecontent.Options{MaxSize: 4}
	for _, tc := range []struct {
		name, content, want string
	}{
		{"at the limit", base64.StdEncoding.EncodeToString([]byte("abcd")), ""},
		{"encoded past the limit", base64.StdEncoding.EncodeToString([]byte("abcdefgh")), "file too large: base64 content exceeds 8 bytes (max decoded size: 4 bytes)"},
		{"decoded past the limit", base64.StdEncoding.EncodeToString([]byte("abcde")), "file too large: 5 bytes exceeds maximum of 4 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filecontent.Read(context.Background(), map[string]any{"content_base64": tc.content}, opts)
			if tc.want == "" {
				require.NoError(t, err)
				assert.Len(t, got.Data, 4)
				return
			}
			fe := refusal(t, err)
			assert.Equal(t, filecontent.TooLarge, fe.Kind)
			assert.Equal(t, tc.want, fe.Error())
		})
	}
}

func TestRead_RequireBytes(t *testing.T) {
	host := newFakeFileHost(t, func(http.ResponseWriter, *http.Request) {})
	budget := newBudget(t)
	var logs bytes.Buffer
	for name, args := range map[string]map[string]any{
		"base64":  {"content_base64": ""},
		"fetched": {"file": fileObject(host.srv.URL + secretPath)},
	} {
		t.Run(name, func(t *testing.T) {
			opts := chatGPT(pinTo(t, host.srv), budget, &logs)
			opts.AcceptsFile = name == "fetched"
			opts.RequireBytes = true
			_, err := filecontent.Read(context.Background(), args, opts)
			fe := refusal(t, err)
			assert.Equal(t, "the content is empty: send the file's bytes", fe.Error())
			assert.Zero(t, budget.Held())

			opts.RequireBytes = false
			got, err := filecontent.Read(context.Background(), args, opts)
			require.NoError(t, err)
			assert.Empty(t, got.Data)
			got.Done()
			assert.Zero(t, budget.Held())
		})
	}
}

func TestOpenAIFetcher_Pin(t *testing.T) {
	assert.Equal(t, []string{"files.oaiusercontent.com"}, filecontent.OpenAIFetcher().Hosts())
	for rawURL, want := range map[string]bool{
		"https://files.oaiusercontent.com/file-1?sig=x":       true,
		"https://FILES.oaiusercontent.com/file-1":             true,
		"https://files.oaiusercontent.com:443/file-1":         true,
		"http://files.oaiusercontent.com/file-1":              false,
		"https://files.oaiusercontent.com:8443/file-1":        false,
		"https://web-sandbox.oaiusercontent.com/file-1":       false,
		"https://files.oaiusercontent.com.evil.example/x":     false,
		"https://evil.example/files.oaiusercontent.com":       false,
		"https://user@files.oaiusercontent.com/file-1":        false,
		"https://files.oaiusercontent.com@evil.example/x":     false,
		"https://169.254.169.254/latest/meta-data":            false,
		"file:///etc/passwd":                                  false,
		"https://files.oaiusercontent.com%2eevil.example/x":   false,
		"https://xn--files-oaiusercontent-com.example/file-1": false,
	} {
		u, err := url.Parse(rawURL)
		assert.Equal(t, want, err == nil && filecontent.OpenAIFetcher().Allows(u), rawURL)
	}
	assert.False(t, filecontent.OpenAIFetcher().Allows(nil))
}

// The production address policy refuses the loopback dial the test pins
// admit, so opening the policy is the only thing that lets the tests above
// reach their fake host.
func TestProductionPolicy_RefusesLoopback(t *testing.T) {
	host := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
	resp, err := pinWith(t, host.srv, filecontent.ProductionPolicy()).Client().Get(host.srv.URL)
	if err == nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, errors.Is(err, surfguard.ErrBlocked), "refused by the address policy: %v", err)
	assert.Zero(t, host.hits.Load())
}

// A fetched file's bytes are counted against the budget from the read until
// Done, and released on every refusal; a call that finds the budget spent is
// refused.
func TestRead_FetchedFileHoldsTheBudget(t *testing.T) {
	host := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pngBytes) })
	sum := sha256.Sum256(pngBytes)
	altered := sha256.Sum256(append([]byte{0}, pngBytes...))

	t.Run("held until done", func(t *testing.T) {
		budget := newBudget(t)
		var logs bytes.Buffer
		got, err := filecontent.Read(context.Background(),
			map[string]any{"file": fileObject(host.srv.URL + secretPath), "sha256": hex.EncodeToString(sum[:])},
			chatGPT(pinTo(t, host.srv), budget, &logs))
		require.NoError(t, err)
		assert.Greater(t, budget.Held(), int64(len(pngBytes)-1), "the bytes stay counted until Done")
		got.Done()
		assert.Zero(t, budget.Held())
		got.Done()
		assert.Zero(t, budget.Held(), "a second Done releases nothing more")
	})
	t.Run("the bytes held are the bytes reserved", func(t *testing.T) {
		budget := newBudget(t)
		var logs bytes.Buffer
		got, err := filecontent.Read(context.Background(),
			map[string]any{"file": fileObject(host.srv.URL + secretPath)},
			chatGPT(pinTo(t, host.srv), budget, &logs))
		require.NoError(t, err)
		defer got.Done()
		assert.Equal(t, int64(cap(got.Data)), budget.Held(), "the buffer is no larger than its reservation")
	})
	t.Run("released on a sha256 mismatch", func(t *testing.T) {
		budget := newBudget(t)
		var logs bytes.Buffer
		_, err := filecontent.Read(context.Background(),
			map[string]any{"file": fileObject(host.srv.URL + secretPath), "sha256": hex.EncodeToString(altered[:])},
			chatGPT(pinTo(t, host.srv), budget, &logs))
		assert.Contains(t, refusal(t, err).Error(), "altered in transit")
		assert.Zero(t, budget.Held())
	})
	t.Run("spent", func(t *testing.T) {
		budget := newBudget(t)
		release, ok := budget.Reserve(budget.Size())
		require.True(t, ok)
		defer release()
		var logs bytes.Buffer
		_, err := filecontent.Read(context.Background(),
			map[string]any{"file": fileObject(host.srv.URL + secretPath)},
			chatGPT(pinTo(t, host.srv), budget, &logs))
		fe := refusal(t, err)
		assert.Equal(t, filecontent.Busy, fe.Kind)
		assert.True(t, strings.HasPrefix(fe.Error(), "Rate limited"), fe.Error())
		assert.Equal(t, budget.Size(), budget.Held(), "only the test's own reservation")
	})
	t.Run("no budget", func(t *testing.T) {
		hits := host.hits.Load()
		var logs bytes.Buffer
		_, err := filecontent.Read(context.Background(),
			map[string]any{"file": fileObject(host.srv.URL + secretPath)},
			chatGPT(pinTo(t, host.srv), nil, &logs))
		assert.Equal(t, filecontent.Busy, refusal(t, err).Kind)
		assert.Equal(t, hits, host.hits.Load(), "nothing is fetched without a budget")
		assertNoURL(t, err, &logs)
	})
	t.Run("a budget smaller than the cap bounds an undeclared body", func(t *testing.T) {
		chunked := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) {
			for range 4 {
				_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		})
		budget, err := filecontent.NewMemoryBudget(2048)
		require.NoError(t, err)
		var logs bytes.Buffer
		_, err = filecontent.Read(context.Background(),
			map[string]any{"file": fileObject(chunked.srv.URL + secretPath)},
			chatGPT(pinTo(t, chunked.srv), budget, &logs))
		fe := refusal(t, err)
		assert.Equal(t, filecontent.TooLarge, fe.Kind)
		assert.Zero(t, budget.Held())
	})
}

// A cap at the largest int64 neither overflows the reservation nor refuses a
// small file.
func TestRead_HugeCap(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		host := newFakeFileHost(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(pngBytes)
			if f, ok := w.(http.Flusher); ok && chunked {
				f.Flush()
			}
		})
		var logs bytes.Buffer
		opts := chatGPT(pinTo(t, host.srv), newBudget(t), &logs)
		opts.MaxSize = math.MaxInt64
		got, err := filecontent.Read(context.Background(), map[string]any{"file": fileObject(host.srv.URL + secretPath)}, opts)
		require.NoError(t, err)
		assert.Equal(t, pngBytes, got.Data)
		got.Done()

		b64, err := filecontent.Read(context.Background(),
			map[string]any{"content_base64": base64.StdEncoding.EncodeToString(pngBytes)}, filecontent.Options{MaxSize: math.MaxInt64})
		require.NoError(t, err)
		assert.Equal(t, pngBytes, b64.Data)
	}
}

func TestNewMemoryBudget(t *testing.T) {
	for _, size := range []int64{0, -1} {
		_, err := filecontent.NewMemoryBudget(size)
		assert.Error(t, err, size)
	}
	b, err := filecontent.NewMemoryBudget(10)
	require.NoError(t, err)
	release, ok := b.Reserve(6)
	require.True(t, ok)
	_, ok = b.Reserve(5)
	assert.False(t, ok, "past the budget")
	assert.Equal(t, int64(6), b.Held())
	release()
	release()
	assert.Zero(t, b.Held(), "release is once")
	_, ok = b.Reserve(-5)
	assert.False(t, ok, "a negative reservation takes nothing, and frees nothing")
	_, ok = b.Reserve(15)
	assert.False(t, ok)
	assert.Zero(t, b.Held())
	_, ok = b.Reserve(10)
	assert.True(t, ok)
}

func TestProperties(t *testing.T) {
	props := filecontent.Properties(map[string]any{"filename": map[string]any{"type": "string"}}, "image")
	assert.Equal(t, map[string]any{"type": "string", "description": "The image content encoded as base64"}, props["content_base64"])
	assert.Equal(t, map[string]any{"type": "string", "description": "Hex SHA-256 of the decoded content; a mismatch uploads nothing"}, props["sha256"])
	assert.Equal(t, filecontent.FileSchema("The image attached in this chat, instead of content_base64"), props["file"])
	assert.Contains(t, props, "filename")

	schema := filecontent.FileSchema("d")
	assert.Equal(t, []string{"download_url", "file_id"}, schema["required"])
	assert.Equal(t, false, schema["additionalProperties"])
	assert.ElementsMatch(t, []string{"download_url", "file_id", "mime_type", "file_name"}, keys(schema["properties"].(map[string]any)))
}

func TestMeta(t *testing.T) {
	assert.Equal(t, mcp.Meta{"openai/fileParams": []string{"file"}}, filecontent.Meta(nil))
	assert.Equal(t, mcp.Meta{"other": true, "openai/fileParams": []string{"file"}}, filecontent.Meta(mcp.Meta{"other": true}))
}

func TestWithoutFile(t *testing.T) {
	served := &mcp.Tool{
		Name: "create_attachment",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": filecontent.Properties(map[string]any{"filename": map[string]any{"type": "string"}}, "file"),
			"required":   []string{"filename"},
		},
		Meta: filecontent.Meta(nil),
	}
	listed := filecontent.WithoutFile(served)
	schema := listed.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	assert.NotContains(t, props, "file")
	assert.Contains(t, props, "content_base64")
	assert.Contains(t, props, "sha256")
	assert.Equal(t, []any{"filename", "content_base64"}, schema["required"])
	assert.Nil(t, listed.Meta, "no meta left")

	// The served definition is untouched.
	assert.Contains(t, served.InputSchema.(map[string]any)["properties"], "file")
	assert.Contains(t, served.Meta, "openai/fileParams")

	t.Run("other meta kept, content_base64 not required twice", func(t *testing.T) {
		tool := &mcp.Tool{
			Name:        "t",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file": map[string]any{}}, "required": []string{"content_base64"}},
			Meta:        filecontent.Meta(mcp.Meta{"keep": "me"}),
		}
		out := filecontent.WithoutFile(tool)
		assert.Equal(t, mcp.Meta{"keep": "me"}, out.Meta)
		assert.Equal(t, []any{"content_base64"}, out.InputSchema.(map[string]any)["required"])
	})
	t.Run("a required file is no longer required", func(t *testing.T) {
		tool := &mcp.Tool{
			Name:        "t",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file": map[string]any{}}, "required": []string{"name", "file"}},
		}
		out := filecontent.WithoutFile(tool)
		assert.Equal(t, []any{"name", "content_base64"}, out.InputSchema.(map[string]any)["required"])
	})
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestChatGPT(t *testing.T) {
	for _, tc := range []struct {
		name, userAgent string
		want            bool
	}{
		{"openai-mcp", "", true},
		{" OpenAI-MCP ", "", true},
		{"", "openai-mcp/1.0.0", true},
		{"", "openai-mcp", true},
		{"", "OpenAI-MCP/1.0.0 (x)", true},
		{"claude-ai", "", false},
		{"claude-ai", "Claude-User/1.0", false},
		{"", "", false},
		{"", "openai-mcp-proxy/1.0", false},
		{"openai", "", false},
		{"", "/openai-mcp", false},
	} {
		assert.Equal(t, tc.want, filecontent.ChatGPT(tc.name, tc.userAgent), "%q %q", tc.name, tc.userAgent)
	}
}
