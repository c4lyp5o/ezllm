# ezllm — OpenAI/Anthropic-compatible LLM router.
#
# Static by design: SQLite is pure-Go (modernc.org/sqlite), so CGO_ENABLED=0
# yields a single static binary. That is what makes a distroless runtime
# possible — no libc, no shell, no package manager for an attacker to reuse.
# README "Build & run" states this intent; keep it true.
#
# Build context needs web/dist because cmd/ezllm embeds it via //go:embed
# (web/embed.go). web/dist IS tracked in git for exactly this reason, so the
# image build needs Go but NOT Node.

# ── Stage 1: build ───────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS builder
WORKDIR /src

# go.sum is the cache key: unchanged deps do not re-download.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath: no local paths in the binary (reproducible + nothing to leak).
# -s -w: drop the symbol/DWARF tables (smaller; symbols live in releases).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w" \
      -o /out/ezllm ./cmd/ezllm

# Confirm the binary really is static — a non-static build silently reintroduces
# a libc dependency that distroless would then break at runtime.
RUN apk add --no-cache file >/dev/null 2>&1 \
 && file /out/ezllm | grep -q "statically linked" \
 && echo "OK: statically linked -> distroless-safe" \
 || { echo "FATAL: binary is not static; distroless runtime will fail"; exit 1; }

# ── Stage 2: runtime ────────────────────────────────────────────────────────
# :nonroot runs as uid 65532; static variant carries CA certs (needed for
# HTTPS to every LLM provider) but NO shell — there is nothing to exec for a
# HEALTHCHECK, so health is probed from outside the container (see README).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/ezllm /app/ezllm

# Reference copy only. The LIVE config is mounted by the operator — see
# docker run notes. Never bake config.yaml or data/ into an image: it holds
# provider keys and the request ledger.
COPY --chown=nonroot:nonroot config.example.yaml /app/config.example.yaml

# Writable home so the app can resolve ~ and write nothing outside /data.
WORKDIR /app

# data_dir in config is repo-relative (./data); this env var OVERRIDES it
# (cmd/ezllm/main.go:66), giving one predictable place to bind-mount.
ENV EZLLM_DATA_DIR=/data
# -addr > EZLLM_ADDR > config.listen (main.go:131). Config defaults to
# loopback, so container users MUST override this to serve anything.
ENV EZLLM_ADDR=0.0.0.0:20129
ENV TZ=Asia/Kuala_Lumpur

VOLUME ["/data"]
EXPOSE 20129

# Distroless has no shell; ENTRYPOINT is the binary, CMD is the config path.
# A missing/invalid config fails the boot loudly (config.Load errors), which is
# correct: this process holds provider keys, so it must never start unconfigured.
ENTRYPOINT ["/app/ezllm"]
CMD ["-config", "/app/config.yaml"]
