# ezllm

Calypso's lightweight LLM router. Single static Go binary, **three passthrough
surfaces** (OpenAI chat · Anthropic messages · OpenAI Responses), namespace
routing across multiple provider accounts, encrypted key storage, and a SQLite
usage ledger that normalizes token accounting across all three protocols.

**Status: M2** — store + crypto + three-surface passthrough, live-verified against
two real providers. Combos/strategies/caps (M4), admin registration API (M3),
compression (M5) and the dashboard (M6) are still ahead.

## Read this first

- `EXPLORING.md` — the codebase tour: what to read, in what order, which
  invariants to verify, and per-milestone file/line landmarks.
- `~/workspace/.hermes/plans/2026-10-05_223000-ezllm-v1-rev4-namespace-compression.md`
  — the plan (rev5, gated). Its §0 probe table is the evidence behind the design.

## Build & run

```bash
make check                          # fmt + vet + test + build
cp config.example.yaml config.yaml  # provider metadata only; add credentials through the dashboard

make run
go run ./cmd/m2verify               # acceptance check against the live ledger
```

Requires **Go 1.27** (`~/.local/go/bin/go`); SQLite is pure-Go
(`modernc.org/sqlite`) so `CGO_ENABLED=0` static builds work — that's what makes
the Docker image distroless-able.

## Endpoints (M2)

| Route | Auth | State |
|---|---|---|
| `GET /healthz` | none | ✅ |
| `GET /v1/models` | bearer or `x-api-key` | ✅ namespaced ids; `?protocol=anthropic` for Anthropic shape |
| `POST /v1/chat/completions` | bearer | ✅ stream + non-stream |
| `POST /v1/messages` | `x-api-key` (or bearer) | ✅ tool use verified live |
| `POST /v1/responses` | bearer | ✅ incl. `response.incomplete` usage |
| `GET /admin/health` | **admin role** | ✅ |
| `GET /admin/usage?group_by=account\|key\|alias\|client\|model\|surface\|day` | **admin role** | ✅ |

## Addressing models

Routing is `<namespace>/<model>`, where the namespace belongs to a **provider
account** (combos have no namespace):

```bash
curl localhost:20129/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"opengo/qwen3.8-flash","messages":[{"role":"user","content":"hi"}]}'
curl localhost:20129/v1/responses -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"opengo/gpt-6-luna","input":"hi"}'
```

A bare model id is **404 by design** — there is no default account, and the same
model id can exist on several of your accounts (`gpt-6-luna` lives on both
`opengo` and `super-ssn`), so guessing would mis-attribute spend. The 404 lists
every namespace that does serve it.

## Token accounting (the reason the ledger exists)

The three surfaces disagree about caching, so raw numbers can't be summed:

| Surface | cached tokens are… | cache-write reported? |
|---|---|---|
| `/v1/chat/completions` | **included** in `prompt_tokens` | no |
| `/v1/messages` | **excluded** from `input_tokens` | yes |
| `/v1/responses` | **included** in `input_tokens` | yes |

Every ledger row stores normalized `tokens_in / tokens_out / tokens_cached_read /
tokens_cached_write` (+ `reasoning_tokens`) plus the verbatim `raw_usage` JSON for
audit. Verified live: a cached repeat reports raw `prompt=2496, cached=2048` and
lands as `tokens_in=448 + cached_read=2048`.

## Config precedence

Command-line flags override `config.yaml`; the global upstream retry policy defaults to
3 retries after the initial attempt per combo hop. A hop is retried only for transient
transport failures, timeouts, `429`, or `5xx`; then combo failover advances to the next
hop with a fresh retry budget. Credentials are managed in SQLite via admin APIs and
dashboard settings. Keep `data/ezllm.sqlite` private and include it in protected
backups. For phone-on-LAN access, use `-addr 0.0.0.0:20129`.

## Security notes

- Provider keys are stored in SQLite as plaintext per the operator's explicit
  testing-build choice; protect the database and its backups with filesystem access controls.
- Client tokens are SHA-256 hashed and dashboard passwords bcrypt-hashed in SQLite; plaintext credentials are never returned after creation/login.
- Only a masked provider-key hint is ever exposed in admin responses or logs.
- The upstream's own credentials replace the client's: `Authorization`, `x-api-key`
  and `Cookie` are stripped from client requests before forwarding.

## Non-goals (v1)

No Anthropic↔OpenAI translation (native passthrough covers all three surfaces),
no GitHub Copilot/OAuth kind yet, no quota scraping beyond `GET /v1/usage`, no
multi-node/Postgres. See EXPLORING.md §7 and the plan's §8.
