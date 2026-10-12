# Anthropic → OpenAI Translation Layer Implementation Plan

> **2026-10-13:** superseded in part by `docs/universal-wire-design.md`. The pairwise
> translators described here remain the CONTRACT for the legacy cells (their bytes are
> pinned by tests), while the three new cells ride the strict surface-neutral codecs.
> `translation.universal_enabled` (default off) is the opt-in.

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** Let Claude Code (an `/v1/messages` client) use OpenAI-surface hops — starting with the free `opencode-go/step-5-preview-free` — by translating the wire protocol per hop, while every non-translated path stays byte-identical to today.

**Architecture:** A new self-contained `internal/translate` package owns all conversion (request bodies, response bodies, SSE streams). The proxy's `attempt()` gains one decision point: when the client surface is anthropic and the hop is *proven* OpenAI-only, dial the hop on its native OpenAI path with a translated body and translate the response back. Eligibility comes from the existing `proto_*` catalog columns, so the feature is inert until a probe sweep marks models — a rollout gate that already exists.

**Tech Stack:** Go (stdlib `encoding/json` only — no new deps), the existing gated mock rig for e2e, the scratch-instance pattern for live canaries.

---

## Why (measured, not assumed)

- `step-5-preview-free` ledger history: **941 × 200 on surface `openai`, 0 successes on `anthropic` ever.** It does not speak `/v1/messages`.
- Claude Code speaks anthropic only (`ANTHROPIC_BASE_URL` + `/v1/messages`), and a protocol-mismatch **400 is not in the failover set** (`failoverStatus`, `internal/proxy/proxy.go:810`), so it reaches the client raw.
- ezllm's stated invariant — "ezllm passes bodies through untranslated" — is exactly why Claude Code can never use step-5 today. **This plan changes that invariant deliberately and narrowly:** translation happens only for hops whose `proto_*` proof says the native path is impossible, and only when the operator has enabled it.

## Invariants (each has a test)

1. **Passthrough identity.** With translation off (or no eligible hop), the bytes leaving ezllm are byte-identical to today. Regression test captures a real request body and asserts the upstream receives it unchanged.
2. **Determinism.** Byte-stable input → byte-stable translated output (fixed key order via ordered struct marshaling, never `map[string]any` re-marshal). This is what keeps upstream automatic prefix caching working (proven on DeepSeek: 78–84% cache-read).
3. **No `cache_control` synthesis.** Anthropic cache markers are dropped in translation; we ride the upstream's automatic prefix cache (write premium 0, verified `cache_creation=0` on non-Anthropic providers).
4. **Ledger honesty.** A translated call is recorded under the **client's** surface (`anthropic`) with the **upstream's** model, and `tokens_in` follows the anthropic convention (INCLUSIVE of cache reads) — see Open Question 2.
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
| `max_tokens` | `max_tokens` | **provider quirk:** tokenrouter `openai/gpt-6.1-sol` 400s without `max_completion_tokens` — handle via account `CustomHeaders`-style per-account knob? NO: config `translation.max_tokens_field` per account is YAGNI; instead send BOTH only for accounts whose `base_url` host is known to need it — see Open Question 3 |
| `stop_sequences` | `stop` | |
| `temperature`, `top_p` | same | pass through when present |
| `stream` | `stream` + `stream_options:{include_usage:true}` | include_usage needed for honest ledger |
| `metadata`, `thinking`, `cache_control`, `anthropic_version` | dropped | documented in code comments; `thinking` → Open Question 4 |
| `model` | replaced by hop's upstream model id | the proxy already swaps `model` at `proxy.go:567-568` — translation replaces that swap for translated hops |

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
7. `usage` absent (provider ignores include_usage): `output_tokens` counted from... nothing reliable — emit the final `message_delta` with `output_tokens:0` and record `usage_absent` in the ledger err-note column? NO — see Open Question 5

**CRLF discipline:** Anthropic SSE frames are `event: <name>\ndata: <json>\n\n`. Test byte-exactness with a golden fixture.

## Design decisions

- **New package `internal/translate/`** — `request.go`, `response.go`, `sse.go`, `types.go`, plus `testdata/*.json` fixtures. Keeps `proxy.go` clean and gives the feature one owner (the repo's extraction rule for shared files).
- **Hook point: `attempt()` (`internal/proxy/proxy.go:594`).** After route selection, if client surface is anthropic AND the hop is translation-eligible, call `translate.Request(body, hopModel)` → dial `SurfacePath(SurfaceOpenAI)` → wrap the response in a `translatingReader` that converts SSE or JSON. The `Result` carries the client's surface for the ledger.
- **Eligibility (who is translation-eligible):** hop's `proto_anthropic` is **false** (proven incompatible) or NULL-and-400-on-anthropic is not enough — only `proto_openai = true` counts as "can serve translated". Both flags come from the existing probe sweep (`ModelSupportsSurface`, `internal/store/catalog.go:318`; probe script `scripts/probe-surface.sh`). **Because `proto_*` is NULL for every model today, deploying this feature changes nothing until the sweep runs.** That is the rollout gate, by construction.
- **Ordering:** native-surface hops keep absolute priority (existing router order). Translated hops are tried only after every native-compatible hop has failed or was skipped — the router's candidate list gains translated hops at the tail.
- **Config:** `translation: {enabled: false}` in `config.yaml` + validation. Default OFF in the first deploy; the operator (Calypso) flips it after the scratch canary passes. Env override `EZLLM_TRANSLATION_ENABLED` follows the existing precedence (flag > env > config > default).
- **No new schema.** `calls.surface` records the client's surface (anthropic) — correct as-is. A `translated` marker needs a column → defer, batch with the usage-convention migration (Open Question 2).

---

## Tasks

### Task 0: Fixtures and contract tests (no runtime code)

**Files:** Create `internal/translate/types.go` (anthropic + openai request/response structs with fixed field order), `internal/translate/testdata/anthropic_request.json`, `testdata/openai_request_golden.json`, `testdata/openai_response.json`, `testdata/anthropic_response_golden.json`, `testdata/openai_sse_stream.txt`, `testdata/anthropic_sse_golden.txt`.

**Step 1:** Hand-write the fixtures from the contract tables above. The golden files ARE the spec — a reviewer can diff them against the Anthropic/OpenAI docs.
**Step 2:** Write `internal/translate/contract_test.go` that loads each fixture pair; it will fail to compile (no functions yet). `go test ./internal/translate/` → FAIL.
**Step 3:** Commit (empty package + fixtures + failing-test placeholders skipped; just fixtures + types): `feat(translate): wire-protocol fixtures and types for anthropic<->openai`.

### Task 1: Request translation (anthropic → openai)

**Files:** Create `internal/translate/request.go`, `internal/translate/request_test.go`.

**Step 1 (failing test):** `TestRequestTranslationGolden` — load `testdata/anthropic_request.json`, run `Request(body []byte, upstreamModel string) ([]byte, error)`, assert output == `testdata/openai_request_golden.json` **byte-for-byte** (determinism invariant, tested on the first day).
Include table cases: string content, multi text blocks, tool_result flatten, assistant tool_use → tool_calls, tools array, tool_choice all 3 shapes, stop_sequences, stream+include_usage, unknown-field rejection is NOT an error (forward-compat: ignore unknown keys silently).
**Step 2:** Run → FAIL (`Request` undefined).
**Step 3:** Implement `Request` using the ordered structs from Task 0. Rules: never mutate the input slice; `json.Marshal` on structs only; tool `arguments` produced by `json.Marshal(parsedInput)` — deterministic because Go sorts map keys.
**Step 4:** `go test ./internal/translate/ -count=1` → PASS. Also `TestDeterminism`: run `Request` twice on the same input, assert identical bytes.
**Step 5:** Commit: `feat(translate): anthropic-to-openai request translation (golden-tested, deterministic)`.

### Task 2: Non-streaming response translation (openai → anthropic)

**Files:** Create `internal/translate/response.go`, `internal/translate/response_test.go`.

**Step 1 (failing test):** `TestResponseTranslationGolden` — `Response(body []byte, clientModel string) ([]byte, error)` maps `testdata/openai_response.json` → `testdata/anthropic_response_golden.json` byte-exact. Table cases: empty choices, refusal finish_reason, missing usage → zeros, tool_calls, both text+tool_calls.
**Step 2:** FAIL. **Step 3:** Implement. **Step 4:** PASS + determinism. 
**Step 5:** Commit: `feat(translate): openai-to-anthropic response translation`.

### Task 3: SSE synthesis (openai chunks → anthropic events)

**Files:** Create `internal/translate/sse.go`, `internal/translate/sse_test.go`.

**Step 1 (failing test):** `TestSSESynthesisGolden` — feed `testdata/openai_sse_stream.txt` (a recorded chunk sequence incl. role first-chunk, text deltas, a tool_call, finish_reason, and a final `usage` chunk) through `NewSSEReader(r io.Reader) io.Reader` and assert the emitted bytes equal `testdata/anthropic_sse_golden.txt` exactly. Plus `TestSSEMidStreamError` (upstream error frame → `event: error` then EOF), `TestSSENoUsage` (no usage chunk → zeros, no crash), `TestSSEDoubleToolCall` (two indexes, blocks interleaved correctly).
**Step 2:** FAIL. **Step 3:** Implement with `bufio.Scanner` + a tiny state machine (indices, open blocks, accumulated `arguments`). No goroutines — a pure `io.Reader` wrapper keeps the proxy's streaming path unchanged (no buffering of the whole stream).
**Step 4:** PASS. **Step 5:** Commit: `feat(translate): anthropic SSE synthesis from openai chunks`.

### Task 4: Config knob + eligibility

**Files:** Modify `internal/config/config.go` (new `Translation` struct + validation + env override), `cmd/ezllm/main.go` (log `translation enabled=true` at boot — a set-but-unread knob looks identical to a working one), Create `internal/translate/eligibility_test.go`.

**Step 1 (failing test):** `TestEligible` — pure function `Eligible(clientSurface Surface, protoAnthropic, protoOpenAI *bool, enabled bool) bool`: requires `enabled && clientSurface==anthropic && protoOpenAI==true && protoAnthropic==false-or-null`. NULL anthropic is allowed (untested native path, but we know translated works).
**Step 2:** FAIL. **Step 3:** Implement. **Step 4:** PASS. **Step 5:** Commit: `feat(translate): config knob and hop eligibility rule`.

### Task 5: Proxy integration (the hook)

**Files:** Modify `internal/proxy/proxy.go` (`attempt()` at :594 — add the decision + body swap + response wrapping; keep `hopKey`, logging, cooldowns untouched), `internal/proxy/proxy_test.go` (passthrough identity test), Create `internal/proxy/translate_e2e_test.go`.

**Step 1 (failing tests):**
- `TestPassthroughUnchangedWhenTranslationOff` — mock upstream records raw bytes; translation disabled; assert request is byte-identical to the client body (Invariant 1).
- `TestTranslatedHopServesAnthropicClient` — mock openai-only upstream; anthropic client request; translation on + eligible; assert client receives a well-formed anthropic message JSON and the upstream received the golden openai body.
- `TestTranslatedHopSSEStreams` — same but `stream:true`; assert the client's SSE bytes equal the golden anthropic stream.
- `TestTranslatedHopFailureStillFailsOver` — translated hop 400s/429s; assert the next hop is tried and park/skip logging fires with the upstream's words (proves Invariant 5).
**Step 2:** FAIL. **Step 3:** Implement the hook. Keep it under ~60 lines in proxy.go — all logic lives in `internal/translate`. 
**Step 4:** Full gate: `gofmt -l`, `go vet ./...`, `go test ./... -count=1` → ALL PASS.
**Step 5:** Commit: `feat(proxy): translate anthropic requests onto proven-openai hops`.

### Task 6: Router integration (ordering + probe-sweep runbook)

**Files:** Modify `internal/router/router.go` (candidate ordering: native first, translated tail), `internal/router/router_test.go`, `docs/API.md` + `README.md` (the invariant change, stated honestly).

**Step 1 (failing test):** `TestNativeHopsPreferredOverTranslated` — combo with one native-compatible hop (down/parked) + one translated-eligible hop; assert order native→translated, and that a healthy native hop always wins.
**Step 2:** FAIL. **Step 3:** Implement. **Step 4:** PASS.
**Step 5:** Write the runbook into the repo docs: how to run the probe sweep to set `proto_*` on opencode-go models (`scripts/probe-surface.sh` + dashboard), and what flips in routing when it runs.
**Step 6:** Commit: `feat(router): translated hops ordered after native; runbook for proto probe sweep`.

### Task 7: Mock-rig e2e + scratch-instance live canary (no prod)

**Step 1:** Extend the battle rig (`~/.hermes/cache/scratch/ezllm-battle/`) with an openai-only mock; run Claude-Code-shaped traffic (fixture bodies from Task 0) through combos; assert success rate + SSE integrity under load.
**Step 2:** Scratch instance (tonight's pattern: online-backup DB copy, loopback port, torn down after — the copy carries plaintext keys) with `translation.enabled=true`; point a real Claude Code env (`ANTHROPIC_BASE_URL=http://127.0.0.1:20199`) at a combo whose hop 0 is `opencode-go/step-5-preview-free`; run one real `explore this repo`. **This is the "see it succeed" moment.**
**Step 3:** Record results in the vault. No commit (verification only).

### Task 8: Ship

**Step 1:** Calypso reviews the diff; commits as `feat(translate): anthropic→openai hop translation (opt-in, proto-gated)`.
**Step 2:** CI → backup (`~/backups/ezllm/`, 0600) → `docker compose create --force-recreate` + `docker start` (the working path from 2026-10-12).
**Step 3:** Prod deploy with `translation.enabled=false` first (behavior identical by construction), then the operator flips the knob via config + one more recreate. Verify: boot log line, `hop` log lines still clean, ledger shows `surface=anthropic, model=step-5-preview-free, status=200` — the first anthropic-surface step-5 200 in the ledger's history.

---

## Verification summary

- Every task: `go test ./<pkg>/ -count=1` green before commit; full suite before integration tasks.
- Golden fixtures = the spec; byte-exact asserts enforce determinism from Task 1 onward.
- Passthrough identity regression runs in the proxy package forever.
- Heavy load only against the mock rig — zero provider spend.
- One live canary on a scratch instance before any prod change; prod flips knob-by-knob.

## Risks

- **Claude Code's SSE parser strictness** — mitigated by golden fixtures copied from real Anthropic transcripts (record one via a claude-platform hop if unsure).
- **Tool-call argument fragmentation** — OpenAI streams `arguments` in arbitrary fragment boundaries; the accumulator must be a plain string concat, never parse-per-fragment.
- **Provider quirks (`max_completion_tokens`)** — see Open Question 3.
- **Prompt-cache invalidation** — determinism test is the guard; any nondeterminism instantly breaks `TestDeterminism`.
- **Body size** — Claude Code sends 1.3 MB bodies; translation adds one unmarshal + one marshal of that size (~tens of ms). Acceptable; measure in Task 7's rig.

## Open questions (need Calypso, not code)

1. **Should translation also serve the `responses` surface?** Recommend NO (YAGNI — no client today).
2. **`tokens_in` convention for translated rows** — record INCLUSIVE (anthropic convention, matches the client's other rows) even though the upstream reports the openai way? Recommend YES + a `translated` marker column **batched with the usage-convention migration** (design doc already committed: `docs/usage-convention-design.md`) — one migration, one rehearsal.
3. **`max_completion_tokens` accounts** (tokenrouter `openai/gpt-6.1-sol` 400s otherwise): per-account config field, or hardcode host quirks? Recommend a tiny per-account `translation_overrides` map in config, only if he actually wants those models translated.
4. **`thinking` blocks**: Claude Code can send extended-thinking params; opencode models ignore them. Recommend: drop silently in M1, revisit if a hop errors.
5. **Streams without usage**: emit zeros honestly or refuse translated streaming for accounts that ignore `include_usage`? Recommend zeros + a log line; the ledger `err`-note column records `usage_absent` only if he wants it.

## Not building (YAGNI)

- OpenAI→Anthropic direction (no client needs it).
- `cache_control` marker synthesis or mapping.
- The responses surface.
- Image content blocks beyond pass-through of base64 → `image_url` data URLs (Claude Code's core traffic is text + tool results; images can be a follow-up if a real client needs them).

---

## Locked decisions (2026-10-12, Calypso sign-off)

All five open questions resolved to the plan's recommendations:

1. **`responses` surface: NOT translated.** YAGNI — no client today. Translation serves anthropic→openai only.
2. **Ledger: `tokens_in` INCLUSIVE for translated rows** (anthropic convention, matches the client's other rows). A `translated` marker column ships **batched with the usage-convention migration** (`docs/usage-convention-design.md`) — one migration, one rehearsal. Until that migration, translated rows are distinguishable only by upstream model.
3. **No per-account `translation_overrides` map.** Accounts needing `max_completion_tokens` (tokenrouter `openai/gpt-6.1-sol`) are simply not translation-eligible in M1; revisit only if those models are actually wanted translated.
4. **`thinking` blocks: dropped silently** in translation. Revisit only if a hop errors on their absence.
5. **Streams without a usage chunk: emit honest zeros + one log line.** No `usage_absent` ledger err-note.
