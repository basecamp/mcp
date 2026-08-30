# eval — structural MCP eval loop

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

## Servers — one loop, three catalogs

The eval package has **no per-server code**: it reads whatever a session lists
and describes. Everything product-specific is one entry in the command's server
registry (`cmd/eval/main.go`), so a second and third server are configuration,
not code.

| Server | Launch | Hermetic? |
|--------|--------|-----------|
| `fake` | in-process catalog (`fake.go`) | yes — the CI fixture |
| `fizzy` | `fizzy-mcp stdio --writes` | yes — dummy `FIZZY_TOKEN`, never reaches a backend |
| `hey` | `hey-mcp stdio` | yes — catalog served from the vendored SDK model |
| `basecamp` | `basecamp-mcp stdio` | **no** — see below |

**`fizzy` and `hey` are hermetic**: `tools/list` + `describe` are served from
each product's vendored catalog, so both run offline at zero cost with a dummy
(or no) token. **`basecamp` is not**: its stdio server authenticates eagerly —
it fetches `authorization.json` before serving the transport — so it needs real
credentials and network. It is wired into the registry as a credentialed /
`--live` target so it composes the instant it can run; making it hermetic is the
cassette player (hillclimb #2), which stubs that startup.

## Run it

```bash
# Hermetic smoke — in-process fake server, deterministic oracle, zero spend,
# gated against a committed baseline:
make eval-smoke

# Real cheap-model run against a product's stdio server. fizzy and hey are
# hermetic (structural only; the server never reaches a backend):
go build -o /tmp/hey-mcp github.com/basecamp/hey-mcp-server/cmd/hey-mcp
go run ./eval/cmd/eval --server hey \
    --server-cmd "/tmp/hey-mcp stdio" \
    --backend cli --models haiku \
    --scenarios eval/testdata/scenarios/hey.json \
    --out eval/results/hey-v0.jsonl

# Catch regressions: compare a fresh run against a prior one. Exits nonzero on
# any score drop or newly-failing scenario — the merge-blocking signal.
go run ./eval/cmd/eval --server hey --server-cmd "/tmp/hey-mcp stdio" \
    --backend cli --models haiku \
    --scenarios eval/testdata/scenarios/hey.json \
    --out /tmp/hey-now.jsonl \
    --baseline eval/results/hey-v0.jsonl
```

The default server command for a known product is resolved on `PATH`
(`fizzy-mcp`, `hey-mcp`, `basecamp-mcp`); override with `--server-cmd` or
`EVAL_<PRODUCT>_CMD`. Backends: `oracle` (deterministic gold, no spend — tests
and CI), `cli` (the local `claude` CLI, no API key needed), `api` (the
Anthropic Messages API, needs `ANTHROPIC_API_KEY`, exact token usage).

## The runs

Cheap model (haiku), one turn each, temp-0, n=1, over each server's committed
corpus. Two structurally distinct product catalogs, one unchanged loop:

```
server  tools/actions            model   pass    params  safety  cost_usd
fizzy   boards, cards            haiku   12/12   12/12   12/12   $0.0165   results/fizzy-v0.jsonl
hey     boxes, contacts,         haiku   12/12   12/12   12/12   $0.0183   results/hey-v0.jsonl
        threads, todos
```

Both clear cleanly and cost under two cents: at this size the loop proves the
machinery and the frugality story across products, not model discrimination —
that is what the harder-scenario hillclimb adds. The point of the second server
is that landing it took zero eval-package changes: the hey corpus
(`testdata/scenarios/hey.json`) was generated straight from hey's own describe
surface, spanning reads, writes, idempotent updates, and destructive deletes
across four domains.

## Regression gate — `--baseline`

Each run's JSONL is the append-only store. `--baseline <prior.jsonl>` compares a
fresh run to it cell by cell, keyed on `(model, scenario_id)`, and classifies
each change:

- **newly-failing** — passed in the baseline, fails now (gates).
- **score-drop** — score fell without crossing the pass line (gates).
- **safety** — respected safety before, violates it now, even at equal score (gates).
- **improved** / **added** / **removed** — reported, never gated (an improvement
  or a corpus edit is not a regression).

Any gating change prints a diff table and exits nonzero, so a catalog, SDK, or
prompt change that quietly degrades routing fails a check instead of merging
unnoticed. Cross-run keying on the catalog/SDK/API SHAs (the design's three-SHA
row, which floats a drop to its layer) rides on adding those fields to the
record — a corpus regenerated from a changed catalog currently surfaces as
added/removed cells rather than a same-cell drop.

## CI

`make eval-smoke` (workflow `.github/workflows/eval.yml`) runs the loop against
the fake server with the oracle backend **and gates it against the committed
baseline** `testdata/results/fake-oracle.jsonl`: no live model calls, no
network, zero cost, and a nonzero exit if the fake catalog surface shifts. The
unit tests cover the generator (determinism, seed sensitivity, distinct-action
sampling, gold validity), the grader (every dimension, type checks, enum,
safety), and the baseline comparison (each regression kind, per-model keying,
corpus edits never gating). The hermetic end-to-end smoke runs in the normal
`make test` job too.

## Hillclimb — deferred, each an independent increment

- **Cassette player** — a record/replay stub for `basecamp-mcp`'s eager
  startup (and, later, real dispatched-result grading), which turns basecamp
  from a `--live` target into a hermetic one.
- **Three-SHA rows** — stamp catalog/SDK/API SHAs on each record so a baseline
  drop keys directly to the layer that moved, and a PR touching a catalog reruns
  only the changed domains' scenarios.
- **Harder scenarios**: distractor tools, paraphrased framings, under-specified
  requests, multi-step tasks — to make the corpus discriminate between models.
- **An LLM judge** for the open-ended value-accuracy cases rules can't score.
- **Prompt caching** on the API backend: the catalog system prompt is static, so
  cache reads collapse the per-scenario input cost.
- **CI with a tiny real-model set** behind a gated secret, for a periodic signal
  rather than per-PR.
