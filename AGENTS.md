# AGENTS.md

Instructions for AI agents (and new maintainers) working on this repository.

## What this is

ezllm — a self-hosted LLM router/gateway: one static Go binary serving three
inference surfaces (OpenAI chat, Anthropic messages, OpenAI Responses) plus
extended endpoints (embeddings, images, audio), with namespace routing, combo
failover, per-model rules, probe-gated wire-protocol translation, staged request
compression, a SQLite usage ledger, and an embedded operator dashboard
(`web/` → `web/dist/` → `go:embed`).

## First hour

1. Read [README.md](README.md) for the product shape, then
   [EXPLORING.md](EXPLORING.md) for the code tour (reading order, invariants,
   per-milestone landmarks).
2. Run `make test-short` before work. This is the local functional gate; CI
   separately runs formatting, vet, the full suite, cross-builds and web build.
3. Design decisions live in `docs/*-design.md` and `docs/*-runbook.md`;
   milestone narrative in `CHANGELOG.md`; dated version bumps in
   `VERSION.md`. Don't restate them from memory — open the doc.

## Hard rules

- **Run the gate.** Run `make test-short` locally. Never report a change as
  tested without it.
- **NEVER run the test suite without `-short`.** Not `go test ./...`, not
  `go test ./internal/server/`, not a single package, not "just to see". The
  perf harnesses (`TestLatencyPercentiles`, `TestRetainedMemory`,
  `TestSlowConsumer`) are not skipped by a package selector, peak ~4.21 GiB
  RSS, and will OOM-kill or hang the shell — an agent hit this on 2026-10-23
  and lost six minutes to a package run that `-short` finishes in 106 s.
  Always use `go test -short …` or `make test-short` locally; do not run
  `make check`, `make test`, `make test-race`, or `make perf` on this machine.
- **Version discipline:** bump `internal/server/version.go` and the
  `Current:` line of `VERSION.md` in the same commit. `version_test.go`
  enforces the pairing; `GET /healthz` serves the constant.
- **`healthz` contract:** exactly two fields — `status` and `version`.
  Adding anything else (counts, paths, uptime) is a regression;
  `TestHealthIsPublic` is the guard.
- **Web changes require `npm run build` in `web/`** — `web/dist/` is
  committed and embedded; forgetting the rebuild ships stale UI.
- **No secrets in commits.** `config.yaml`, `data/`, `*.sqlite*` are
  ignored deliberately. Live proofs read credentials from env/secret files.
- **No weakened tests.** A new fix ships with a test that fails without it;
  existing assertions are never relaxed to make a failure pass.

## Memory constraint (important on this box)

The full suite peaks at **~4.21 GiB RSS** (`TestLatencyPercentiles`). In a
4 GiB cgroup — e.g. a Hermes worker scope — that is an OOM kill regardless of
free machine RAM or `GOMEMLIMIT` (soft limit, doesn't cap RSS). Use:

```bash
make test-short    # functional suite, ~0.18 GiB peak, skips perf harnesses
```

CI runs the full suite separately. Details and measured numbers:
CONTRIBUTING.md § Memory.

## Layout map

```
cmd/ezllm/          main (config, flags, run, in-process bridge wiring)
cmd/m2verify/       live-ledger acceptance checker
internal/config/    config.yaml + EZLLM_* environment loading
internal/server/    HTTP layer: routes, auth, admin API, healthz, version
internal/router/    combo resolution, strategies, rules eligibility, pins
internal/proxy/     attempt/commit split, retry, failover, usage sniff, TTFT
internal/compress/  staged engines (session_dedup, rtk, headroom, lite, caveman, ansi, fold, jsonmin, tool_ref, blobs)
internal/translate/ wire-protocol IR + per-wire codecs (anthropic↔openai↔responses)
internal/store/     SQLite schema + queries (migrations carry schema_version)
internal/provider/  provider kinds, presets, catalog probes, quota oracles
internal/registration/ key testing: paced probe chain, protocol sweep
internal/rules/     token caps + allowed-hours windows
internal/ratelimit/ per-account request limiter
internal/jsonbody/  single-parse request document
internal/logx/      structured logging setup and the first-boot banner
internal/{claudeweb,geminiweb,githubcopilot}/  web-protocol providers
internal/claudebridge/  Claude subscription surface (loopback-only): CLI spawn, body injection, SSE
internal/codexbridge/   ChatGPT subscription surface (loopback-only): Codex app-server JSON-RPC, lockdown, exec fallback
internal/agybridge/     Antigravity subscription surface (loopback-only): headless agy spawn, preflight, lockdown
web/                React+Vite dashboard; web/tests/ = Playwright drivers
tests/              Python live-proof scripts (not part of go test)
scripts/            operator utilities: backup_sqlite.py (online backup), ezllm-mac.sh (macOS installer + launchd agent)
docs/               design records + runbooks (decisions, not status)
```

## Conventions

- Go 1.27, pure-Go SQLite (`modernc.org/sqlite`), static builds
  (`CGO_ENABLED=0`). Tabs, double quotes, `gofmt` canonical.
- Tests are **in-package** `_test.go` (toolchain constraint — external test
  trees are not used; `tests/` holds only Python live proofs).
- Commit style: Conventional-lite, lowercase, single-line subject
  (`fix:` dominates; `feat:`, `docs:`, `chore:`, `test:`, `perf:`).
- Schema changes bump `schema_version` via store migrations and ship with the
  matching docs/API.md field updates.
- Errors on the wire are OpenAI-shaped
  (`{"error":{"type","message","code"}}`); refusals that name their reason
  (rules → 429, pin → 400 pinned_surface) are deliberate — keep them.

## Docs hygiene

- Cite code as `file.go` plus the symbol (`proxy.go` `Forward`), not a line
  number: line landmarks drift with every edit. Verify a symbol exists before
  citing it.
- Zero stale markers (`TBD`/`TODO`/`⏳`) in `*.md`.
- JSON examples in docs/API.md come from live responses, not memory.
- README = front door (what/how/where); CHANGELOG = milestone narrative;
  VERSION.md = dated version history; design docs = decisions.

## Logging

The contract, the level table and the measured cost are in
[`docs/logging.md`](docs/logging.md). The rules that must not drift:

- **stdout only.** No log file, no rotation, no third-party logger. The
  platform captures stdout; the ledger is the durable record.
- **`slog` evaluates arguments before it checks the level.** Any `Debug` whose
  arguments cost more than a field read (a join, a sprintf, a slice walk) sits
  behind `if log.Enabled(ctx, slog.LevelDebug)`. Measured: 17.7 ns filtered with
  cheap args, 255.8 ns and 2 allocs with one computed arg.
- **A new log line needs a test** that proves it appears at its level and is
  filtered out at the level below. Bite it: remove the call, watch the test
  fail.
- **Never log a credential, body or prompt** — not even a hint. Auth refusals
  name the path and the reason.
- **The happy path allocates nothing for logging.** No per-request string is
  built unless a line is actually emitted.
