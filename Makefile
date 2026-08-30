.PHONY: build test test-race vet tidy clean ready

# Build from the reviewed module graph only, never mutate it (see
# basecamp-mcp-server's Makefile for the full rationale). tidy opts back in.
export GOWORK := off
export GOFLAGS := -mod=readonly
GO := go

build:
	$(GO) build -trimpath ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

tidy:
	GOFLAGS=-mod=mod $(GO) mod tidy

clean:
	rm -rf bin

ready: vet test-race build

# Run the structural eval loop as a hermetic smoke: the in-process fake server
# with the deterministic oracle backend, over a PINNED corpus, compared against
# a committed baseline. No network, no model spend. The pinned corpus is what
# makes the gate real: the committed golds are graded against the current fake
# catalog, so a surface change that invalidates a gold under an unchanged
# scenario id — a renamed action, an added required param, a changed enum —
# scores 0 and fails as a newly-failing regression (nonzero exit). Regenerating
# the corpus instead would let the oracle's answers drift with the schema and
# hide exactly that. Real model runs use the eval command with --backend cli/api.
.PHONY: eval-smoke
eval-smoke:
	$(GO) run ./eval/cmd/eval --server fake --backend oracle \
		--scenarios eval/testdata/scenarios/fake.json \
		--out /tmp/eval-smoke.jsonl \
		--baseline eval/testdata/results/fake-oracle.jsonl
