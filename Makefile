GO  ?= go
BIN := bin/ezllm

.PHONY: build run test vet fmt check clean

build:
	$(GO) build -o $(BIN) ./cmd/ezllm

run:
	$(GO) run ./cmd/ezllm -config config.yaml

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

check: fmt vet test build

clean:
	rm -rf bin
