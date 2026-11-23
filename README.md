# mcp

37signals MCP toolkit: the elements our MCP servers share, extracted from the
product instances that proved them. Sibling of the `sdk` and `cli` meta-repos.

[basecamp-mcp-server](https://github.com/basecamp/basecamp-mcp-server) and
[hey-mcp-server](https://github.com/basecamp/hey-mcp-server) are thin dispatch
layers over their product SDKs, exposing **domain gateway tools** — one MCP
tool per domain, `{"action", "params"}` dispatch, an in-band `describe` action,
read-only filtering, fail-closed domain narrowing. Building the second instance
duplicated the machinery under that surface. This repo is where the duplicated
parts live once, so the third instance (Fizzy) starts from a seed instead of a
copy.

## The two-instance proof rule

Code moves here only after **two product instances have proven it by
duplication** — the third validates it. Nothing lands speculatively: if only
one server runs it (today: hosted HTTP transport and the OAuth resource-server
plumbing, which exist only in basecamp-mcp-server), it stays in that server
until a second instance forces it to converge or diverge. Extraction is a move
of reviewed code, not a rewrite.

## Dependency direction

Products depend on `github.com/basecamp/mcp`. This module never imports a
product SDK or server — no `basecamp-sdk`, no `hey-sdk`. Anything that needs a
product client belongs in the product server, behind an interface defined here.

## Planned packages

Arriving as the extraction sequence lands (see the program board):

| Package | Extracted from | Contents |
|---------|----------------|----------|
| `mcptest` | both servers' test suites | in-memory MCP client/server wire-test harness, snapshot testing with `-update` |
| `gateway` | hey-mcp-server `internal/server`, basecamp-mcp-server `internal/tools` registry | gateway dispatch conventions, read-only and domain filtering over a small catalog interface; `UpstreamFailure`, an upstream 5xx, timeout or network failure answered in the server's own words by read, write or send, with the product's request id and never its body, URL or error text (basecamp-mcp-server#416's words, product-neutral; first consumed by fizzy-mcp-server, hey-mcp-server and hey-cli) |
| `catalog` | hey-mcp-server `internal/catalog` | Domain/Operation types and the behavior-model + OpenAPI join, product-parameterized (tool prefix, domain specs, embedded model snapshot) |
| `result` | basecamp-mcp-server `internal/tools` (result_shape.go, result_cap.go), hey-mcp-server `internal/server/wire_shape.go`; first consumed by hey-mcp-server and fizzy-mcp-server | structuredContent always a JSON object, the serialized-size result cap with page/offset continuations, title mirroring; one receiving middleware applies all three, and pages the tools a server names (`PagingMiddleware`): a record's rich text by `content_offset`, a page-based listing by `offset` within its page, in basecamp-mcp-server's dialect (#307, #349) |
| `auth/exchange` | basecamp-mcp-server `internal/auth/exchange` | RFC 8693 delegation-exchange client for a hosted server: client_secret_post and private_key_jwt client auth, the terminal (re-authorize) vs infra (retry, never re-consent) error fork, allowlisted OAuth error codes with credential-redacted descriptions, optional DPoP proofs and the nonce retry |
| `auth/upstream` | basecamp-mcp-server `internal/upstream` | the error type for credentialed endpoints whose text never renders its cause (fixed reason, status, allowlisted code, scrubbed description), bounded Retry-After and lifetimes, and `upstreamtest`'s hostile-endpoint catalog |
| `confirm` | hey-mcp-server `internal/server` (confirm.go, send.go), hey-cli `internal/mcpserver` (send.go, server.go) | the person's confirmation before a tool acts: HMAC-signed state on a rotating keyring (`kid`), bound to the tool, its arguments, an identity, a grant and a digest of what the person was shown, with an expiry; the one-boolean form elicitation and its answer (decline, cancel and false are a "no"); which clients can be asked (2026-07-28 on any transport, legacy only on stdio); a tools/list filter; per-process claims that forget at their TTL. What each tool asks and does with a "no" stays in the product |
| `grant` | fizzy-mcp-server `internal/hosted` (per-grant surfaces), hey-mcp-server `internal/server/scopes.go` (in-band refusal); first consumed by all three hosted servers at once | a tool surface fitted to the request's OAuth grant: tools/list names only what the grant can run, and a call it can't run returns `isError` with the product's reconnect sentence and `_meta["mcp/www_authenticate"]` asking for granted ∪ needed (no HTTP 403 step-up); scope implications, the challenge's quoting; `Declare`, which stamps each governed tool's `_meta.securitySchemes` with the scopes it needs, the other half of what ChatGPT relinks from. What each tool needs and what the refusal says stay in the product |
| `requestlog` | hey-mcp-server and fizzy-mcp-server `internal/hosted` (requestlog.go) | the one line per MCP message a hosted server answers (method, tool, client, protocol, grant, session, duration, outcome; never arguments or results), plus the request id (`X-Request-ID`) that joins it to the HTTP request and, through an outbound transport, to the app's log of the calls made for it |

Per-product forever: domain curation and blurbs, vendored SDK model snapshots,
SDK clients and adapters, env var surfaces, product-specific tools and
resources.

## Development

The Go toolchain is pinned in `.mise.toml` and `go.mod`. Activate mise before
building:

```bash
eval "$(mise hook-env)" && make ready
```

`make ready` runs vet, the race-enabled tests, and the build — run it before
committing.
