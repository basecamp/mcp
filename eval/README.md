# eval — structural MCP eval loop (v0)

A hermetic, rule-graded eval for the gateway MCP servers built on this toolkit
(basecamp / hey / fizzy). It reads a live server's own wire surface as the
spec, generates natural-language scenarios from it, asks a cheap model to pick
the right `{tool, action, params}`, and grades the answer by rule — correct
tool + action, params valid against the catalog schema, and safety annotations
respected. No product backend, LLM judge, or cassette is involved.

Because the eval speaks each catalog's own `{action, params}` vocabulary over
the wire, it is product-agnostic: it lives in the toolkit and is inherited by
every server built on it for free. Point it at the in-process fake catalog or
at any product's real stdio server.

## What v0 proves

The whole loop turns end to end on the free, deterministic layer:

1. **Spec from the server** (`spec.go`) — `tools/list` + the gateway `describe`
   action yield every action's identity, safety annotations, and param/body
   schema. Served from the catalog, never the product API, so it is hermetic:
   the fizzy server runs with a dummy token and is never allowed to reach a
   backend.
2. **Deterministic, seedable generation** (`scenario.go`) — a pure function of
   `(specs, seed, N)`: no clock, no network, no global rand. It samples distinct
   actions weighted toward the destructive and idempotent classes (the ones an
   agent most needs to get right) and renders a natural-language framing plus a
   gold resolution. The corpus is cached and hand-checkable at
   `testdata/scenarios/fizzy.json`.
3. **A real cheap-model turn** (`model.go`, `prompt.go`) — the static catalog is
   the system prompt (cacheable), the per-scenario framing is the user turn. The
   model returns `{tool, action, params}`; the runner parses it, tolerating
   fences and prose.
4. **Rule grading** (`grade.go`) — exact tool+action match, JSON-Schema-level
   param validation (required present, no unknowns, enums honored, types match),
   and the read-only safety rule: a read/lookup framing must never resolve to a
   destructive action.
5. **A scored report with a measured cost** (`report.go`) — a scenario×model
   table plus per-model pass / params / safety rates and a total `$`. Cost is a
   deterministic function of the prompt and answer (`EstimateTokens` × a
   published price table), so the frugality metric is reproducible and costs
   nothing to compute; an API backend's exact usage overrides the estimate.

Frugality is a first-class metric, not an afterthought: the report always prints
tokens and dollars, and the only spend in the whole loop is the per-scenario
model turn.

## Run it

```bash
# Hermetic smoke — in-process fake server, deterministic oracle, zero spend:
go run ./eval/cmd/eval --server fake --backend oracle --n 12

# Real cheap-model run against the fizzy stdio server (structural only; the
# server runs with a dummy token and never reaches a backend):
go build -o /tmp/fizzy-mcp github.com/basecamp/fizzy-mcp-server/cmd/fizzy-mcp
go run ./eval/cmd/eval --server fizzy \
    --server-cmd "/tmp/fizzy-mcp stdio --writes" \
    --backend cli --models haiku \
    --scenarios eval/testdata/scenarios/fizzy.json \
    --out eval/results/fizzy-v0.jsonl
```

Backends: `oracle` (deterministic gold, no spend — tests and CI), `cli` (the
local `claude` CLI, no API key needed), `api` (the Anthropic Messages API,
needs `ANTHROPIC_API_KEY`, exact token usage).

## The v0 fizzy run

12 scenarios over fizzy's 8 tools / 45 actions, cheap model, one turn each,
temp-0, n=1. See `results/fizzy-v0.jsonl`:

```
model       pass      params    safety    in_tok     out_tok    cost_usd
haiku       12/12     12/12     12/12     19199      291        $0.0165
```

Under two cents ($0.0165) of estimated model spend proves the whole loop
turns and the cost story is real. A capable model clears this small, unambiguous corpus cleanly:
v0 proves the machinery, not model discrimination — that is what the hillclimb
adds.

## CI

`make eval-smoke` (workflow `.github/workflows/eval.yml`) runs the loop against
the fake server with the oracle backend: no live model calls, no network, zero
cost. The unit tests (`eval_test.go`) cover the generator (determinism, seed
sensitivity, distinct-action sampling, gold validity) and the grader (every
dimension, type checks, enum, safety), and the hermetic end-to-end smoke runs in
the normal `make test` job too.

## Hillclimb — deferred, each an independent increment

- **Harder scenarios**: distractor tools, paraphrased framings, under-specified
  requests, multi-step tasks — to make the corpus discriminate between models.
- **A live-API layer**: cassettes (record/replay) and then real calls, grading
  the dispatched result, not just the proposed call.
- **An LLM judge** for the open-ended cases rules can't score.
- **Prompt caching** on the API backend: the catalog system prompt is static, so
  cache reads collapse the per-scenario input cost (the report already counts it
  per scenario, which is the uncached upper bound).
- **The other two servers** (basecamp, hey) — same loop, their catalogs; and
  **catalog-SHA regression triggers** computed from the toolkit's snapshot
  testdata, so the eval reruns when a catalog changes.
- **CI with a tiny real-model set** behind a gated secret, for a periodic signal
  rather than per-PR.
