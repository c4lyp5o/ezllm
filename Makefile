GO  ?= go
BIN := bin/ezllm

.PHONY: build static run test test-race vet fmt check verify clean

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

clean:
	rm -rf bin
