GO  ?= go
BIN := bin/ezllm

# Local test runs carry CI's memory envelope (.github/workflows/test.yml):
# GOMEMLIMIT/GOGC keep the Go heap in the same budget the runner uses.
# Honest scope (measured 2026-10-17): this does NOT cap RSS — the perf
# harness (TestLatencyPercentiles) peaks ~4.21 GiB with any limit, because
# the limit is soft and the traffic is transient. Under a memory-capped
# cgroup (e.g. a 4 GiB worker scope) the full suite can still be OOM-killed
# — use `make test-short` there (skips the harnesses: 0.18 GiB peak).
test test-race bench perf: export GOMEMLIMIT := 3GiB
test test-race bench perf: export GOGC := 50

.PHONY: build static run test test-short test-race vet fmt check verify bench perf clean

build:
	$(GO) build -o $(BIN) ./cmd/ezllm

# Static, no-CGO build — this is what the Docker image ships. modernc.org/sqlite
# (pure Go) is what makes it possible; mattn/go-sqlite3 would break it.
static:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/ezllm
	@file $(BIN) | grep -q "statically linked" && echo "✓ statically linked" || echo "✗ NOT static"

run:
	$(GO) run ./cmd/ezllm -config config.yaml

# -count=1 / -timeout 15m mirror CI exactly — cached results were masking
# changes, and the percentile harness legitimately takes minutes.
test:
	$(GO) test -count=1 -timeout 15m ./...

# Memory-constrained environments (4 GiB cgroups): skips the percentile /
# retained-memory harnesses, still runs every functional test.
test-short:
	$(GO) test -short -count=1 ./...

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
# measurements. Not skipped by -short, so do not pass it. Needs real memory:
# see the envelope note at the top of this file.
perf:
	$(GO) test ./internal/server/ -run 'TestLatencyPercentiles|TestRetainedMemory|TestSlowConsumer|TestClientCancel|TestUpstreamNotReported' -v -count=1

clean:
	rm -rf bin
