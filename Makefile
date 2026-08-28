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
