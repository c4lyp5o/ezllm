# ezllm — OpenAI/Anthropic-compatible LLM router.
#
# Static by design: SQLite is pure-Go (modernc.org/sqlite), so CGO_ENABLED=0
# yields a single static binary. That is what keeps the BUILD free of libc — the
# runtime stage below carries glibc only because the optional Claude
# subscription surface execs the host-mounted Claude Code CLI (see below):
# no shell, no package manager for an attacker to reuse.
# README "Build & run" states this intent; keep it true.
#
# Build context needs web/dist because cmd/ezllm embeds it via //go:embed
# (web/embed.go). web/dist IS tracked in git for exactly this reason, so the
# image build needs Go but NOT Node.

# ── Stage 1: build ───────────────────────────────────────────────────────────
# Runs on the BUILD host's platform and cross-compiles for the target
# (TARGETARCH, set by buildx): an arm64 image never compiles under QEMU, and the
# runtime stage below has no RUN step, so it needs no emulation either.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETARCH
WORKDIR /src

# go.sum is the cache key: unchanged deps do not re-download.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath: no local paths in the binary (reproducible + nothing to leak).
# -s -w: drop the symbol/DWARF tables (smaller; symbols live in releases).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w" \
      -o /out/ezllm ./cmd/ezllm

# Confirm the binary really is static — a non-static build silently reintroduces
# a libc dependency that distroless would then break at runtime.
RUN apk add --no-cache file >/dev/null 2>&1 \
 && file /out/ezllm | grep -q "statically linked" \
 && echo "OK: statically linked -> distroless-safe" \
 || { echo "FATAL: binary is not static; distroless runtime will fail"; exit 1; }

# ── Stage 2: runtime ────────────────────────────────────────────────────────
# distroless/base (NOT /static): the Claude subscription surface spawns the
# official Claude Code CLI, which is bind-mounted from the host and is a
# glibc-dynamic ELF (interpreter /lib64/ld-linux-x86-64.so.2, links libc/librt/
# libpthread/libdl/libm — measured with ldd). On distroless/static that exec
# fails with the misleading "no such file or directory" because the loader is
# absent. On arm64 the mounted CLI must be the arm64 build (loader
# /lib/ld-linux-aarch64.so.1, also in base). base carries glibc + CA certs + NSS (DNS resolution needs libnss_dns,
# which ldd does not list) but still no shell and no package manager. The ezllm
# binary itself remains static.
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=builder /out/ezllm /app/ezllm

# Reference copy only. The LIVE config is mounted by the operator — see
# docker run notes. Never bake config.yaml or data/ into an image: it holds
# provider keys and the request ledger.
COPY --chown=nonroot:nonroot config.example.yaml /app/config.example.yaml

# Writable home so the app can resolve ~ and write nothing outside /data.
WORKDIR /app

# data_dir in config is repo-relative (./data); this env var OVERRIDES it
# (cmd/ezllm/main.go:84), giving one predictable place to bind-mount.
ENV EZLLM_DATA_DIR=/data
# -addr > EZLLM_ADDR > config.listen (main.go:184). Config defaults to
# loopback, so container users MUST override this to serve anything.
ENV EZLLM_ADDR=0.0.0.0:20129
ENV TZ=Asia/Kuala_Lumpur

# Claude subscription surface: off unless EZLLM_CLAUDE_BRIDGE=on, and it
# expects the official CLI at /opt/claude/claude and the subscription login at
# /claude-sub (claudebridge.go defaults), bind-mounted by your compose.yml. Never
# bake either into the image.
#
# ChatGPT subscription surface: off unless EZLLM_CODEX_BRIDGE=on; it expects the
# official Codex CLI release at /opt/codex (binary /opt/codex/bin/codex, a static
# musl build — any base runs it — with its code-mode host beside it) and the
# isolated ChatGPT login at /codex-sub (codexbridge.go defaults).
# Same rule: mounted, never baked. docs/codex-bridge-runbook.md.

VOLUME ["/data"]
EXPOSE 20129

# Distroless has no shell; ENTRYPOINT is the binary, CMD is the config path.
# A missing/invalid config fails the boot loudly (config.Load errors), which is
# correct: this process holds provider keys, so it must never start unconfigured.
ENTRYPOINT ["/app/ezllm"]
CMD ["-config", "/app/config.yaml"]
