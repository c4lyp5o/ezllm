# ezllm

Calypso's lightweight LLM router. Single Go binary, OpenAI-compatible surface,
two upstreams (OpenCode Go + Alibaba Token Plan), SSE byte-passthrough with a
SQLite usage ledger.

**Status: M1** — skeleton + health. `/v1/chat/completions` returns 501 until M2.

## Read this first

`EXPLORING.md` — the codebase tour (what to read, in what order, what to verify).
`~/workspace/.hermes/plans/2026-10-05_151500-ezllm-v0-opencode-alibaba.md` — the plan
(this repo's source of truth, incl. the RECON CORRECTION block with verified endpoints).

## Build & run

```bash
make check                 # fmt + vet + test + build
cp config.example.yaml config.yaml   # then set the env vars it references:
export EZLLM_TOKEN_HERMES=...        # any random string: your client token
export OPENCODE_GO_API_KEY="$(cat ~/.hermes/secrets/opencode-go.txt)"
export BAILIAN_CODING_API_KEY="$(cat ~/.hermes/secrets/bailian-coding-plan.txt)"
make run
```

## Endpoints (M1)

| Route | Auth | State |
|---|---|---|
| `GET /healthz` | none | ✅ |
| `GET /v1/models` | bearer | ✅ (lists aliases) |
| `POST /v1/chat/completions` | bearer | 501 until M2 |

Precedence for listen address: `-addr` > `EZLLM_ADDR` > `config.listen`
(loopback `127.0.0.1:20129` by default — bind `0.0.0.0` only via env when
phone-testing on LAN).

## Non-goals (v0)

No Anthropic translation, no quota scraping, no `/responses` API, no dashboard UI, no Docker.
See EXPLORING.md §7.
