# Anthropic → OpenAI translation — design record (2026-10-12)

> **Superseded in part by `docs/universal-wire-design.md` (2026-10-13).** Every client surface
> now reaches every proven wire through surface-neutral codecs, translated hops are ordinary
> combo members (no longer a tail after native hops), and one switch, `translation.enabled`
> (default **on**), gates it all (`translation.universal_enabled` is a deprecated alias). This
> record keeps the original rationale, invariants and contracts; the byte contracts below are
> pinned by tests. The operating procedure is `docs/translation-runbook.md`.

**Goal:** Let Claude Code (an `/v1/messages` client) use OpenAI-surface hops — starting with the free `opencode-go/step-5-preview-free` — by translating the wire protocol per hop, while every non-translated path stays byte-identical to today.

**Architecture:** A new self-contained `internal/translate` package owns all conversion (request bodies, response bodies, SSE streams). The proxy's `attempt()` gains one decision point: when the client surface is anthropic and the hop is *proven* OpenAI-only, dial the hop on its native OpenAI path with a translated body and translate the response back. Eligibility comes from the existing `proto_*` catalog columns, so the feature is inert until a probe sweep marks models — a rollout gate that already exists.

**Tech Stack:** Go (stdlib `encoding/json` only — no new deps), the existing gated mock rig for e2e, the scratch-instance pattern for live canaries.

---

## Why (measured, not assumed)

- `step-5-preview-free` ledger history: **941 × 200 on surface `openai`, 0 successes on `anthropic` ever.** It does not speak `/v1/messages`.
- Claude Code speaks anthropic only (`ANTHROPIC_BASE_URL` + `/v1/messages`), and a protocol-mismatch **400 is not in the failover set** (`failoverStatus` in `internal/proxy/proxy.go`), so it reaches the client raw.
- ezllm's stated invariant — "ezllm passes bodies through untranslated" — is exactly why Claude Code can never use step-5 today. **This plan changes that invariant deliberately and narrowly:** translation happens only for hops whose `proto_*` proof says the native path is impossible, and only when the operator has enabled it.

## Invariants (each has a test)

1. **Passthrough identity.** With translation off (or no eligible hop), the bytes leaving ezllm are byte-identical to today. Regression test captures a real request body and asserts the upstream receives it unchanged.
2. **Determinism.** Byte-stable input → byte-stable translated output (fixed key order via ordered struct marshaling, never `map[string]any` re-marshal). This is what keeps upstream automatic prefix caching working (proven on DeepSeek: 78–84% cache-read).
3. **No `cache_control` synthesis.** Anthropic cache markers are dropped in translation; we ride the upstream's automatic prefix cache (write premium 0, verified `cache_creation=0` on non-Anthropic providers).
4. **Ledger honesty.** A translated call is recorded under the **client's** surface (`anthropic`) with the **upstream's** model, and `tokens_in` follows the anthropic convention (INCLUSIVE of cache reads) — see locked decision 2.
5. **Translation never widens the failure set.** A translated hop that fails classifies through the existing `classifyHop`/`failoverStatus` machinery unchanged; park keys stay per-model (shipped 2026-10-12).

## The contracts

### Request: anthropic → openai chat

| Anthropic field | OpenAI field | Notes |
|---|---|---|
| `system` (string or `[{type:"text",text}]`) | first `{role:"system"}` message | preserve order before all user messages |
| `messages[].content` string | `content` string | trivial case first |
| `[{type:"text",text}]` blocks | concatenated text | join with `\n` only when >1 block |
| `[{type:"tool_result",tool_use_id,content}]` | `{role:"tool", tool_call_id, content}` | content may be string or blocks → flatten to text (Claude Code's main payload) |
| assistant `[{type:"tool_use",id,name,input}]` | `tool_calls:[{id,type:"function",function:{name,arguments: json(input)}}]` | `arguments` must be a STRING of compact JSON — `json.Marshal` of a map is already key-sorted in Go, which is deterministic ✓ |
| `tools:[{name,description,input_schema}]` | `tools:[{type:"function",function:{name,description,parameters:input_schema}}]` | verbatim schema |
| `tool_choice:{type:"auto"|"any"|"tool",name}` | `tool_choice:"auto"|"required"|{type:"function",function:{name}}` | `none` → `"none"` |
| `max_tokens` | `max_tokens` | **provider quirk:** tokenrouter `openai/gpt-6.1-sol` 400s without `max_completion_tokens` — handle via account `CustomHeaders`-style per-account knob? NO: config `translation.max_tokens_field` per account is YAGNI; instead such accounts are not translation-eligible (locked decision 3) |
| `stop_sequences` | `stop` | |
| `temperature`, `top_p` | same | pass through when present |
| `stream` | `stream` + `stream_options:{include_usage:true}` | include_usage needed for honest ledger |
| `metadata`, `thinking`, `cache_control`, `anthropic_version` | dropped | documented in code comments; `thinking` dropped (locked decision 4) |
| `model` | replaced by hop's upstream model id | translation replaces the per-hop `model` swap for translated hops |

### Response (non-streaming): openai → anthropic message

| OpenAI | Anthropic |
|---|---|
| `id` | `id` (prefix `msg_` if missing) |
| `choices[0].message.content` | `content:[{type:"text",text}]` |
| `choices[0].message.tool_calls[]` | `content:[{type:"tool_use",id,name,input: json(arguments)}]` (appended after text blocks) |
| `choices[0].finish_reason`: `stop`→`end_turn`, `length`→`max_tokens`, `tool_calls`→`tool_use`, `content_filter`→`refusal`, `function_call`→`tool_use` | `stop_reason` |
| `usage.prompt_tokens/completion_tokens` | `usage.input_tokens/output_tokens` (map 1:1; anthropic has no total field) |
| — | `type:"message"`, `role:"assistant"`, `model:<client-visible model>`, `stop_sequence:null` |

### Response (SSE): openai chunks → anthropic events

The hard part. Emit in this exact order (Claude Code's parser is strict):

1. On first chunk: `message_start` `{message:{id,role:"assistant",content:[],model,stop_reason:null,usage:{input_tokens:<from include_usage or 0>,output_tokens:1}}}`
2. When text delta arrives: `content_block_start` `{index:0,content_block:{type:"text",text:""}}` once, then `content_block_delta` `{index:0,delta:{type:"text_delta",text}}` per chunk
3. When a tool_call delta arrives: `content_block_start` `{index:N,content_block:{type:"tool_use",id,name,input:{}}}` on first fragment, then `content_block_delta` `{index:N,delta:{type:"input_json_delta",partial_json}}` per fragment (accumulate `function.arguments` string fragments)
4. On `finish_reason`: `content_block_stop` for each open index, then `message_delta` `{delta:{stop_reason,stop_sequence:null},usage:{output_tokens}}`, then `message_stop`
5. Anthropic clients tolerate `ping` events — emit one on connect (cheap, matches real Anthropic)
6. Upstream error frame / transport cut mid-stream: emit `event: error` with `{type:"error",error:{type:"api_error",message}}` then close — never a bare TCP close
7. `usage` absent (provider ignores include_usage): `output_tokens` counted from... nothing reliable — emit the final `message_delta` with `output_tokens:0` and record `usage_absent` in the ledger err-note column? NO — honest zeros + a log line (locked decision 5)

**CRLF discipline:** Anthropic SSE frames are `event: <name>\ndata: <json>\n\n`. Test byte-exactness with a golden fixture.

## Design decisions

- **One package, `internal/translate/`,** owns all conversion (request bodies, response bodies, SSE streams), keeping the proxy clean.
- **Hook point: the proxy's per-hop body preparation** (`prepareHopBody` in `internal/proxy/proxy.go` today). When the hop's wire differs from the client's surface, the body is translated, the hop is dialled on its own wire, and the response is converted back. The `Result` carries the client's surface for the ledger.
- **Eligibility:** only a wire the probe sweep PROVED (`proto_* = true`, read through `ModelSupportsSurface` in `internal/store/catalog.go`) counts as "can serve translated"; an unprobed model is never translated. The sweep runs from the key test and the dashboard.
- **Ordering (original):** translated hops came after every native hop. *Changed 2026-10-13:* they are ordinary combo members ordered by the strategy.
- **Config (original):** `translation.enabled`, default off for the first deploy. *Changed:* default on; `EZLLM_TRANSLATION_ENABLED` overrides (flag > env > config > default).
- **No new schema.** `calls.surface` records the client's surface (anthropic) — correct as-is. Translated rows are marked by `calls.translated` (schema v12).

---

## Locked decisions (2026-10-12, Calypso sign-off)

1. **`responses` surface: NOT translated** at the time. *Changed 2026-10-13:* the universal wire matrix translates every surface pair.
2. **Ledger: `tokens_in` INCLUSIVE for translated rows** (anthropic convention, matches the client's other rows). The `translated` marker column shipped in schema v12 (`calls.translated`).
3. **No per-account `translation_overrides` map.** Accounts needing `max_completion_tokens` (tokenrouter `openai/gpt-6.1-sol`) are simply not translation-eligible in M1; revisit only if those models are actually wanted translated.
4. **`thinking` blocks: dropped silently** in translation. Revisit only if a hop errors on their absence.
5. **Streams without a usage chunk: emit honest zeros + one log line.** No `usage_absent` ledger err-note.
