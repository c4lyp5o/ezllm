GO  ?= go
BIN := bin/ezllm

.PHONY: build static run test test-race vet fmt check verify bench perf clean

build:
	$(GO) build -o $(BIN) ./cmd/ezllm

# Static, no-CGO build — this is what the Docker image ships. modernc.org/sqlite
# (pure Go) is what makes it possible; mattn/go-sqlite3 would break it.
static:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/ezllm
	@file $(BIN) | grep -q "statically linked" && echo "✓ statically linked" || echo "✗ NOT static"

run:
	$(GO) run ./cmd/ezllm -config config.yaml

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

check: fmt vet test build

# Acceptance check against the LIVE ledger (run with the server up).
verify:
	$(GO) run ./cmd/m2verify

# Phase-separated benchmarks (see docs/perf-baseline-m1.md). Every one runs
# with -benchmem; -run XXX keeps the tests out of the run.
bench:
	$(GO) test ./internal/proxy/... ./internal/server/... -run XXX -bench . -benchmem -count=1

# The latency-percentile harness and the retained-memory / lifecycle
# measurements. Not skipped by -short, so do not pass it.
perf:
	$(GO) test ./internal/server/ -run 'TestLatencyPercentiles|TestRetainedMemory|TestSlowConsumer|TestClientCancel|TestUpstreamNotReported' -v -count=1

clean:
	rm -rf bin
