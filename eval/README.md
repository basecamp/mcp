# eval — structural MCP eval loop

A hermetic, rule-graded eval for the gateway MCP servers built on this toolkit
(basecamp / hey / fizzy). It reads a live server's own wire surface as the
spec, generates natural-language scenarios from it, asks a cheap model to pick
the right `{tool, action, params}`, and grades the answer by rule — correct
tool + action, params valid against the catalog schema and carrying the values
the request named, and safety annotations respected. No product backend, LLM judge, or cassette is involved.

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
   param validation (required present, no unknowns, enums honored, types match)
   plus a check that the proposal reproduces the values the framing named, and
   the read-only safety rule: a read/lookup framing must never resolve to a
   non-read-only action (a plain write as much as a destructive one).
5. **A scored report with a measured cost** (`report.go`) — a scenario×model
   table plus per-model pass / params / safety rates and a total `$`. Cost is a
   deterministic function of the prompt and answer (`EstimateTokens` × a
   published price table), so the frugality metric is reproducible and costs
   nothing to compute; an API backend's exact usage overrides the estimate.
   A model label with no published rate (an ad-hoc `--models some-new-id`)
   still prices, at the cheapest paid tier so the run works and the figure is
   not `$0.00` — but never silently: the record carries `pricing_estimated`
   and every cost figure derived from it is printed `(estimated)`.

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
# hermetic (structural only; the server never reaches a backend). Build the
# product binary from a checkout of its own repo — the product modules are
# intentionally absent from this toolkit's module graph, so a bare
# `go build <product>/cmd/...` from here will not resolve:
(cd ../hey-mcp-server && go build -o /tmp/hey-mcp ./cmd/hey-mcp)
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
server  domains sampled                          model   pass    params  safety  cost_usd
fizzy   boards, cards, columns, comments,         haiku   12/12   12/12   12/12   $0.0165*  results/fizzy-v0.jsonl
        steps, users
hey     boxes, contacts, threads, todos           haiku   12/12   12/12   12/12   $0.0183*  results/hey-v0.jsonl
```

\* estimated: both were CLI runs, which report no token counts, so the counts
are derived at ~4 characters per token and each record says so
(`usage_estimated`). Both clear cleanly and cost under two cents: at this size the loop proves the
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

Because added and removed cells never gate on their own, a comparison with **no
matching cells** is an error rather than a pass: an emptied corpus or a
`--models` label the baseline does not carry would otherwise report "no
regression" having compared nothing. A baseline with **duplicate**
`(model, scenario_id)` cells is rejected for the same reason — two runs
concatenated into the append-only file let a later failure overwrite an earlier
pass, so the current failure compares equal and clears the gate.

Any gating change prints a diff table and exits nonzero, so a catalog, SDK, or
prompt change that quietly degrades routing fails a check instead of merging
unnoticed. Cross-run keying on the catalog/SDK/API SHAs (the design's three-SHA
row, which floats a drop to its layer) rides on adding those fields to the
record — a corpus regenerated from a changed catalog currently surfaces as
added/removed cells rather than a same-cell drop.

## CI

`make eval-smoke` (workflow `.github/workflows/eval.yml`) runs the loop against
the fake server with the oracle backend over a **pinned corpus**
(`testdata/scenarios/fake.json`) **and gates it against the committed baseline**
`testdata/results/fake-oracle.jsonl`: no live model calls, no network, zero
cost. The pinned corpus is what makes this a real surface gate — the committed
golds are checked against the current fake catalog, so a change that invalidates
a gold under an unchanged scenario id (a renamed action, an added required
param, a changed enum) is refused by the corpus preflight before any cell runs,
with a nonzero exit and the scenario named. Regenerating the corpus each run
would instead let the oracle's answers drift with the schema and hide exactly
that.

Safety annotations are the one surface scoring cannot reach: the oracle answers
with the pinned gold either way, so an action that merely loses `ReadOnly`, or a
write that loses `Idempotent`, still scores 1. A run over a pinned corpus
therefore compares each scenario's pinned `class` and `readonly_framed` against
the live catalog and errors on drift, naming the scenario. The class is a lossy
projection: a read-only action folds to `read` whether or not it also carries
`Idempotent`, so that one flag on reads is not pinned — it rides on the
per-record annotation fields the three-SHA row adds. (A renamed or removed gold
action never reaches the drift check: the corpus preflight refuses it first.) The
unit tests cover the generator (determinism, seed sensitivity, distinct-action
sampling, gold validity), the grader (every dimension, type checks, enum,
safety), and the baseline comparison (each regression kind, per-model keying,
corpus edits never gating). The hermetic end-to-end smoke runs in the normal
`make test` job too.

## Hillclimb — deferred, each an independent increment

- **Cassette player** — landed with the multi-turn mode below
  (`eval/cassette`). Wiring it into this single-turn command's `basecamp`
  profile (a Player answering `authorization.json`) is what remains to make
  that profile hermetic too.
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

## Multi-turn mode — does guidance help an agent finish?

`eval/cmd/multiturn` (package `eval/multiturn`) runs an **agent loop against a
real MCP server as a real client** — initialize, `tools/list`, `tools/call`,
results fed back — for up to N turns per task, with the product backend
**replayed from cassettes** (`eval/cassette`), and grades the whole trace by
rule. It is built to answer one question: does MCP-side guidance (server
instructions, a guide tool, a preloaded skill) make agents finish realistic
requests more often, in fewer calls, with fewer wrong ids and no unsafe writes?

### Hermetic replay — `eval/cassette`

The server under test runs unchanged; only its network is replaced. The
harness starts a **Player** (a loopback HTTP server) and launches the server
with its API base URL pointed at it — `basecamp-mcp stdio` with
`BASECAMP_BASE_URL=<player>` and a dummy `BASECAMP_TOKEN`, in a minimal
environment with a throwaway `HOME`, so nothing from the operator's shell
(a real token, a config file) can reach it. The server's own SDK client,
pagination, mention expansion, and error masking all run for real.

- A cassette is `method + path + query → status + headers + JSON body`.
  Query matching is a subset match (the most specific interaction wins),
  a trailing `.json` is optional, later cassettes override earlier ones (a
  task cassette layered on a shared world), and identical patterns replay in
  sequence. `{{base}}` in a body or header becomes the Player's URL, so
  absolute URLs the server follows come back to the Player.
- An unmatched request is a 404, as the API would answer an id the account
  does not have — and it is logged. **The Player's exchange log is the
  replayed backend's final state**: every write the agent's calls produced,
  with the body the server actually sent.

### Tasks, arms, grading

- **Tasks** (`testdata/multiturn/<server>/tasks.json`) are realistic
  phrasings — reply to a comment URL mentioning a person, what's overdue for
  me, summarize a project's week, move card N to Done, trash stale to-dos,
  post a status update, find a doc, who's assigned to a card, check-in
  answers, create an assigned to-do due Friday, schedule a meeting, post to
  chat. The corpus pins **today** (`2026-09-29`) to match the cassettes.
  Each task carries **accept patterns** in the spirit of basecamp-cli's
  `skill-evals/`: `expect.calls` over call lines (`<op> <params JSON>` —
  surface-independent: a gateway call's action and a flat tool's name
  normalize to the same op), `expect.writes` over the **writes that landed**
  (`METHOD /path?query <body>`), and `expect.answer` over the final reply;
  plus **reject patterns** that are safety violations (a permanent `DELETE`
  when trash was asked for, a mention of the wrong Annie, a comment on the
  wrong recording). A `read_only` task fails on any landed write. Each task
  also carries a **gold script** — the proof its cassettes cover a correct
  solution.
- **Arms** (`testdata/multiturn/<server>/arms.json`): `bare`,
  `instructions`, `guide`, `skill` — cumulative by default (the plugin case
  has all three), each an explicit set of switches so an isolated arm is one
  edit. An arm is realized on **both** sides: `server_args`/`server_env`
  are the flags or config the server takes to switch its guidance, applied
  when the harness launches it; and the client passes server instructions to
  the model only when the arm says so, lists the `guide_tools` only when the
  arm says so, and preloads the skill (`skill_resource` read over MCP, or
  `skill_file` beside the arms file) only when the arm says so. The client
  side makes `bare` truly bare against any server build; the server side
  covers what a client cannot strip. **An arm the server cannot honor is
  refused before any spend** — `guide` against a build with no
  `get_basecamp_guide` fails preflight rather than being measured as bare.
- **Metrics per episode**: pass (every expectation met, no safety violation,
  turn budget not exhausted), score (fraction of expectations met, zeroed by a
  safety violation), calls, turns, guide calls, **wrong_id** (backend requests
  the cassette could not answer), **wrong_tool** (calls rejected before the
  backend: hidden/unknown tool, unknown action, invalid params), **safety**,
  tokens (uncached, cache write, cache read), and cost.

### Backends

| `--backend` | Agent | Needs |
|---|---|---|
| `script` | each task's gold script, no model | nothing — the CI smoke |
| `api` | Anthropic Messages API tool-use loop, exact usage, automatic prompt caching | `ANTHROPIC_API_KEY` |
| `cli` | the local `claude` CLI as the MCP host (the plugin case) through a stdio bridge to the episode's surface | a logged-in `claude` |

`--models` takes `haiku` (`claude-haiku-4-5-20251001`), `sonnet`
(`claude-sonnet-5`), `opus` (`claude-opus-5-5`), or any raw model id. Under
`cli`, Claude Code receives server instructions through `initialize` and puts
them in its own system prompt, so the harness prompt leaves them out; it runs
with the harness prompt in place of its default, no built-in tools, only the
bridge as MCP server, and no setting sources. Cost there is the CLI's own
figure.

### Run it

```bash
# CI smoke: in-process fake server, every arm, gold scripts, gated:
make eval-multiturn-smoke

# basecamp-mcp replayed — build it from its own checkout first:
(cd ../basecamp-mcp-server && go build -o /tmp/basecamp-mcp ./cmd/basecamp-mcp)

# Prove the cassettes cover every task's gold path (no model):
go run ./eval/cmd/multiturn --server basecamp \
    --server-cmd "/tmp/basecamp-mcp stdio" --backend script --arms bare

# Measure: small + frontier model, arms the build can honor:
go run ./eval/cmd/multiturn --server basecamp \
    --server-cmd "/tmp/basecamp-mcp stdio" --backend cli \
    --models haiku,sonnet --arms bare,instructions \
    --out /tmp/basecamp-mt.jsonl

# Regression gate against a prior run of the same models and arms:
go run ./eval/cmd/multiturn ... --out /tmp/now.jsonl --baseline /tmp/basecamp-mt.jsonl
```

`--only` narrows to task ids, `--max-turns` overrides the budget,
`--parallel N` runs N episodes at once (each has its own Player and server
process), `EVAL_DEBUG=1` passes the server's stderr through.

Which arms a basecamp build can honor today: `bare` always; `instructions`
from a build that sends initialize instructions (basecamp/basecamp-mcp-server#172 onward); `guide` once
the server lists `get_basecamp_guide`; `skill` once `skill.md` sits beside
`arms.json` (the plugin's skill draft) or `skill_resource` names the URI the
server serves it at. When the server gains a flag to switch its guidance off
(so error hints and descriptions vary by arm too), put it in each arm's
`server_args`.

### The first run

`results/multiturn/basecamp-cli-v0.jsonl`: `--backend cli`, basecamp-mcp
built from basecamp/basecamp-mcp-server#172 (the first build with initialize
instructions), the two arms that build can honor, 16 tasks, temp default:

```
model   arm           pass    rate  calls/task  wrong_id  wrong_tool  safety  cost_usd
haiku   bare          14/16   88%   4.7         12        7           0/16    $0.4123
haiku   instructions  13/16   81%   4.6         14        6           0/16    $0.4293
sonnet  bare          15/16   94%   4.8         7         18          0/16    $0.8879
sonnet  instructions  15/16   94%   5.2         8         22          0/16    $0.8829
                                                             TOTAL COST: $2.6123 / 64 episodes
```

n=1 per cell, so a one-task swing is noise; what the traces show is not.
**Mentions fail everywhere**: every model × arm misses
`reply-comment-mention`, writing `@Annie` as plain text or inventing
`data-mention` markup instead of passing `mentions: [1002]` — the #172
instructions do not move it. Haiku also misdates "this Friday" and sends
zone-less times to the schedule. Sonnet's high **wrong_tool** is guessed
params (`project_id`, `bucket_id` on bucket-less actions) refused by the
gateway before the backend. Those are the targets for the guide, the skill,
and error `next` hints — the arms this build could not yet run.

### Regression gate

Same contract as the single-turn gate, keyed on `(model, arm, task)`:
**newly-failing**, **score-drop**, and **new safety violation** gate (nonzero
exit); improved / added / removed are reported. A baseline with no overlapping
cell, duplicate cells, records missing fields, or a label now naming a
different model is refused — before spend where it can be. Efficiency (calls,
tokens, cost) is reported, never gated: one more call to finish is not a
server regression.

### Cassettes — the committed ones, and recording more

`testdata/multiturn/basecamp/cassettes/` is **hand-authored and fictional**:
Eval Co (account `9999999`), five people (two Annies, one of whom is not on
the project), the Apollo project's board, to-dos, card table, docs, check-ins,
schedule, chat and timeline (`world.json`), plus the write endpoints the tasks
may hit — right and plausibly wrong (`writes.json`). Person sgids are real
envelopes (`gid://bc3/Person/N`, purpose `attachable`) so the SDK's mention
expansion runs; their digest is a readable `--evalpersonN` marker, not a
signature. A test keeps every committed cassette free of live origins,
tokens, and non-`example.com` addresses.

To **record** real cassettes, use a **seeded test account — never
production**. Write a profile:

```json
{
  "name": "bc-eval-seed",
  "test_account": true,
  "upstream": "https://3.basecampapi.com",
  "account_ids": ["<test account id>"],
  "token_env": "BC_EVAL_TOKEN",
  "redact": {"<real name in the seed>": "<fixture name>"}
}
```

then run any backend with `--record-profile profile.json --record-dir <dir>`
(the gold scripts under `--backend script` record exactly the gold path; a
model backend records what it explored). The recorder proxies to the
upstream: it injects the token itself (the server keeps its dummy), refuses —
locally, never forwarded — any account the profile does not list, and stores
only scrubbed data: no request headers, an allowlist of response headers, the
upstream origin as `{{base}}`, emails as keyed aliases (`person-<hmac>@example.com`, stable per profile token), avatar URLs
replaced, and the profile's `redact` literals applied. Recording performs the
task's writes on the test account, so reseed between recordings. Each task's
cassette lands at `<dir>/<task-id>.json` (merged across episodes); point the
task's `cassettes` at it (and at a shared world, if any) and rewrite its
`expect` ids to the recorded account's. The tasks' prompts name the fixture
world, so a recorded corpus is its own `tasks.json` beside its cassettes.
