# AGENTS.md - mcp

37signals MCP toolkit: shared machinery for our MCP servers
(basecamp-mcp-server, hey-mcp-server, and instances to come).

## Rules

- **Two-instance proof rule.** Code lands here only after two product
  instances have proven it by duplication. No speculative abstraction.
- **Dependency direction.** Products depend on this module; this module never
  imports a product SDK or server (`basecamp-sdk`, `hey-sdk`).
- **Extraction, not rewrite.** Moves of reviewed code, adapted minimally.

## Commands

```bash
make ready            # vet + race tests + build; run before committing
make test             # go test ./...
```

## Toolchain (mise)

Go is pinned in `.mise.toml` (and `go.mod`). Activate mise in-process before
running make — an unactivated shell may resolve a different `go`:

```bash
eval "$(mise hook-env)" && make ready
```
