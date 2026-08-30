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
# with the deterministic oracle backend. No network, no model spend — proves
# the loop turns. Real model runs use the eval command with --backend cli/api.
.PHONY: eval-smoke
eval-smoke:
	$(GO) run ./eval/cmd/eval --server fake --backend oracle --n 12 --out /tmp/eval-smoke.jsonl
