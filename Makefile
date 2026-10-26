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

.PHONY: build build-debug static run test test-short test-quiet test-race vet fmt check verify bench perf clean

# Default build is what ships: static, no symbol/DWARF tables, no local paths.
# Measured 2026-10-26: 26.6 MB plain vs 18.5 MB with these flags. CGO_ENABLED=0
# works because modernc.org/sqlite is pure Go; mattn/go-sqlite3 would break it.
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/ezllm

# Symbols kept (`go tool nm`, debuggers). Nothing ships from this target.
build-debug:
	$(GO) build -o $(BIN) ./cmd/ezllm

# Same flags as `build`, plus the static-link check the Docker image relies on.
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

# Desktop-friendly local gate. `go test ./...` defaults to -p = NumCPU
# packages in parallel, and each test binary uses GOMAXPROCS threads — on a
# 4-core box that is up to 16 runnable threads and the machine stops
# responding. This pins the whole tree to cores 0-1, yields CPU/IO to
# interactive work, and runs one package at a time. It still skips the
# percentile harnesses. Use THIS locally; CI runs the full suite.
test-quiet:
	taskset -c 0-1 nice -n 19 ionice -c 3 env GOMAXPROCS=2 \
		$(GO) test -short -count=1 -p 1 -timeout 15m ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

# The full gate (: full test suite + build) is CI's job. Running it locally
# saturates every core and can OOM a 4 GiB worker. Guarded on purpose: set
# EZLLM_ALLOW_FULL_CHECK=1 only on an uncapped shell.
check:
	@if [ "$$EZLLM_ALLOW_FULL_CHECK" != "1" ]; then \
		echo "refusing full 'make check' locally: it runs every package in parallel and the percentile harness."; \
		echo "use 'make test-quiet' (or 'make test-short'); CI runs the full gate."; \
		echo "override only on an uncapped shell: EZLLM_ALLOW_FULL_CHECK=1 make check"; \
		exit 1; \
	fi
	$(MAKE) fmt vet test build

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
