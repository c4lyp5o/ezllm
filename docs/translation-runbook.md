# Translation runbook — anthropic clients on openai-only hops

ezllm's native contract is passthrough: bodies leave the box byte-identical to
what the client sent (model swap aside). Translation is the **one deliberate
exception**, and it is double-gated so it can never happen by accident:

1. **Operator knob** — `translation.enabled` in `config.yaml`, **default `true`**
   (global enable, 2026-10-12). The default is seeded before the YAML decode, so
   an absent key keeps it and an explicit `enabled: false` still wins;
   `EZLLM_TRANSLATION_ENABLED=false` is the no-rebuild rollback lever (flag >
   env > config > default). The effective state is logged at boot:
   `translation enabled=true|false`. Since 2026-10-22 this is the **one switch**
   for every translated wire, the universal matrix included.
2. **Proven capability** — the hop's probed/curated `proto_*` facts must say
   `proto_openai = true` AND `proto_anthropic` false-or-NULL. Both flags are
   **NULL until a probe sweep marks them or an operator curates them**, so a
   fresh deploy (or a database where the sweep never ran) makes the feature
   inert even with the knob on.

## What a translated hop does

The client keeps speaking Anthropic (`/v1/messages`, e.g. Claude Code); the hop
is dialed on its native OpenAI `/chat/completions` path with a translated body.
Responses — streaming and not — are translated back into Anthropic shapes.
Ledger rows stay on the **client's** surface (`anthropic`) with the **upstream's**
model. Design and wire contracts: `docs/translation-design.md`.

## Ledger attribution (schema v12)

Every call served by a hop whose WIRE differed from the client's surface carries
`calls.translated = 1` — the anthropic→openai-chat leg and responses-wire hops
alike. One marker, one meaning; a row you can COUNT, not infer from
surface+model. Two token facts ride with the marker and they are part of the
column's meaning:

- **`tokens_in` is the vendor's INCLUSIVE total** for translated rows. The
  synthesized anthropic usage envelope passes OpenAI's `prompt_tokens` through
  as `input_tokens`, and OpenAI counts the cached prefix inside it. This is a
  deliberate deviation from the ledger's cold-token convention, pinned by
  `TestTranslatedUsageStaysInclusive`; changing it means changing the marker's
  documented meaning too.
- **`tokens_cached_read` stays 0** — the translator does not carry OpenAI's
  cache split.

Consequence for caps: usage buckets sum `tokens_in`, so translated rows count
their cached prefix against daily caps. On the free/prepaid hop pool that is
the conservative direction (refuse a little early rather than overspend).

The dashboard renders the marker as a `⇄ translated` tag under the model on the
recent-calls table, the requests explorer, and the live feed; `/admin/requests`
and `/admin/overview` rows expose it as `translated: true`.

**Pins still constrain the dispatch wire.** A translated combo hop dials
OpenAI, so an `openai` pin is satisfied; an `anthropic` or `responses` pin
still refuses the whole combo. Native/direct pin checks are unchanged, and
the route retains the client's Anthropic surface for attribution. With the
knob OFF, proven-incompatible hops are dropped before checking their pins,
while an untested wrong-surface pinned hop still returns `400 pinned_surface`.

## Curated capability verdicts (`PUT /admin/accounts/{id}/model-protocol`)

The probe sweep cannot mark every hop — an edge that stonewalls probe traffic
always records UNKNOWN — so an operator can record a capability fact by hand:

    PUT /admin/accounts/{id}/model-protocol
    {"model": "step-5-preview-free", "proto_openai": true, "proto_anthropic": false,
     "note": "edge refuses /v1/messages; the openai wire works"}

- The model id travels in the **body**, not the path: 493 of 622 catalog ids
  contain `/` (the same rule as model-pin).
- Each `proto_*` field is three-state: absent keeps the recorded value, `null`
  clears it to untested, `true`/`false` records the verdict.
- `note` is **required** on a verdict write. It is the audit trail that makes a
  curated fact auditable instead of a silent override.
- A curated write stamps `proto_source='curated'` and leaves `proto_tested_at`
  NULL — nothing was probed, so no probe timestamp is fabricated. In other
  words `proto_tested_at` always means "last probed at", and `proto_source`
  says who wrote the verdicts (`probe` | `curated` | empty for legacy rows).
- Curated rows are **probe-protected**: `SetModelProto` / `SetModelProtos` skip
  them, so a later sweep cannot silently undo the operator's fact and a
  translation-eligibility verdict stays stable.
- `{"model": "...", "release": true}` hands the row back to the sweep: the
  verdicts stay as recorded, `proto_source`/`proto_note` clear, and the next
  probe write applies again.
- A catalog re-sync preserves provenance along with the verdicts.
- 404 when the model is not in the account's catalog (typo protection);
  curation never creates rows.

## Running the probe sweep (this is the rollout gate)

Capability flags come from the dashboard-triggered **ProtocolSweep**
(Models page → protocol sweep), which samples `ProbeMaxModels` (default 6)
models per account × 3 surfaces and writes `proto_openai` / `proto_anthropic` /
`proto_responses` / `proto_tested_at`. Only a verdict is written: a 429, 5xx or
timeout leaves the stored value as it was (a re-sweep in bad weather never wipes
a proven capability back to `NULL`), and a 429 ends the sweep. Probes are not
ledgered. `anthropic-compatible` accounts skip the
openai probe and are marked `proto_openai=false` (chat clients reach them through
the Messages translation); `claude-web` is probed normally — its bridge serves
`/chat/completions` only.

What flips when the sweep runs, in order:

1. Nothing, while `translation.enabled=false` — probed-incompatible hops are
   dropped from combos exactly as before.
2. With the knob ON, a hop marked `proto_anthropic=false, proto_openai=true`
   stops being dropped for anthropic traffic and instead becomes a
   **translated tail candidate**: it is tried only after every native-compatible
   hop has failed or was skipped. A healthy native hop always wins.
3. Hop order otherwise does not change; a translated hop that fails still
   parks/fails over through the ordinary machinery (`classifyHop`,
   per-model parks, `hop skipped` / `hop failed` log lines).

Caveats, learned live (see the ezllm skill):

- **Raise the sample to cover the hops you care about.** The sweep samples N
  models per account; an un-sampled hop stays `NULL` (= untested = native-only
  routing). To aim it: `POST /admin/accounts/{id}/sync` with
  `{"models":["<id>",…],"max_models":N}` — the scope applies to the **sweep
  only**; the catalog refresh always writes the full fetched catalog (handing
  the filtered list to `UpsertModels` used to delete every unnamed row —
  command-code 87→3 in prod, fixed 2026-10-12). The deployed binary predates
  `max_models` and rejects unknown body fields.
- **A 429/timeout mid-sweep means UNKNOWN, never unsupported** — the sweep will
  not silently mark a working model incompatible.
- An account whose group blocks a surface (403 "this group does not allow")
  only gets its SAMPLED models marked; raise the sample or curate the hop out.

## Enabling (after the sweep passes)

```yaml
# config.yaml
translation:
  enabled: true
```

or for the container / a canary: `EZLLM_TRANSLATION_ENABLED=true`
(`docker compose create --force-recreate` + `docker start`).

## Verifying it works

- Boot log shows `translation enabled=true`.
- Send one request through a combo whose translated-eligible hop is the
  openai-only model (e.g. hop 0 `opencode-go/step-5-preview-free` with no
  healthy native hop ahead of it) from an anthropic client.
- The ledger row must read `surface=anthropic`, `model=step-5-preview-free`
  (the upstream's model), `status=200` — the first anthropic-surface 200 for
  that model in the ledger's history.

## Rolling back

Flip the knob off and restart. Nothing else persists: the `proto_*` facts stay
(true facts about the upstream), and routing reverts to dropping
probed-incompatible hops.

## Known non-goals (locked 2026-10-12)

- No `responses`-surface translation (no client today).
- No OpenAI→Anthropic direction.
- No `cache_control` synthesis — the upstream's automatic prefix cache does the
  work (translation is byte-deterministic to protect it).
- `thinking` blocks are dropped silently; revisit only if a hop errors.
- Direct `<namespace>/<model>` requests are NOT translated in M1 — combos only.
- Accounts needing `max_completion_tokens` (tokenrouter `openai/gpt-6.1-sol`)
  are not translation-eligible in M1.

## Universal wire matrix (2026-10-13) — the three NEW cells

Design: `docs/universal-wire-design.md`. Adds **chat→messages**, **responses→chat** and
**responses→messages** through the strict codecs, plus the fixed fallback ordering for any
client whose native wire a hop provably cannot serve.

**Enabling:** on by default, under the same `translation.enabled` /
`EZLLM_TRANSLATION_ENABLED` switch as every other translated wire (merged 2026-10-22).

The former matrix switch, `translation.universal_enabled` /
`EZLLM_TRANSLATION_UNIVERSAL_ENABLED`, is **deprecated** and logs a warning at boot. It still
works as a rollback: an explicit `false` in config turns translation off, and the env var
applies when `EZLLM_TRANSLATION_ENABLED` is unset. A wrong-typed value still fails boot naming
the field.

**Usage convention on a Messages destination:** `auto` (the default) reads the upstream's
`input_tokens` as exclusive of cache, as the ledger does, so an unconfigured Anthropic account
serves chat and Responses clients. Set `usage_convention` to `total` (schema v13,
`PATCH /admin/accounts/{id}` with `{"usage_convention":"total"}`) for an upstream that counts
cache inside `input_tokens` (tokenrouter does); left at `auto`, such an account over-reports
`prompt_tokens` to the client by the cached amount, exactly as its ledger rows already do.

**What you get when it is on:** requests on the new cells are translated through the strict
IR codecs (a feature that would change the answer refuses with a reason at **zero upstream
spend**, and the combo moves on to its next hop), responses and
streams come back in the client's own shape, and error envelopes are re-shaped into the client's
family. Ledger rows keep the `translated` marker and the inclusive `tokens_in` exception.

**Stream outcomes (all cells):** a translated stream that ends in a late error frame or without a
terminal event is recorded in the ledger error column (`translated stream failed` /
`translated stream truncated`) while the delivered status stays 200 (that is what the client
saw), and the hop is not marked good. A translated hop is an ordinary member of the combo
strategy: it is ordered, rotated and marked good on exactly the same terms as a native one.

**Rollback:** `EZLLM_TRANSLATION_ENABLED=false`, or `translation.enabled: false` + restart.
This turns off every translated wire; there is no matrix-only switch any more. No data migration is involved; the curated `responses_models` list keeps working
independently.

## Responses-wire hops (chat client → /v1/responses hop)

Separate companion feature (`docs/responses-wire-design.md`): some models'
**chat** path refuses tools+reasoning (tokenrouter `gpt-6.1-sol`) while their
Responses path handles both. The curated list is the opt-in, and each entry is
a **regex matched fully against `<namespace>/<model_id>`**:

```yaml
translation:
  enabled: true
  responses_models:
    - "space-stationine/gpt-6\\.1-sol"        # one hop, one family pattern
    - "tokenrouter/openai/gpt-6\\.1-sol"      # a plain id matches itself only
    - "vercel-ai-gateway/openai/gpt-6\\.1-sol.*"   # covers -fast variants
```

- Patterns are **anchored at both ends**, so a bare `sol` matches nothing —
  it cannot turn a typo into a fleet-wide wire change. An entry that does not
  compile is a **boot failure naming the entry**.
- **Mark only what needs it.** A hop with a working chat path loses native
  fidelity on the responses wire, so a blanket `.*sol.*` is a tax, not a fix:
  the ledger decides (a model whose chat calls 400 on the tools/reasoning
  signature is a candidate). Probing a model on all three surfaces is what
  tells you which one it actually needs.
- A marked hop is dialed on `/v1/responses` with a translated body (chat →
  responses), and the response translates back (JSON and SSE). The client
  keeps speaking chat; the ledger row keeps the client's surface and the
  upstream's model, and carries `translated=true` (the marker covers every
  translated wire — see "Ledger attribution").
- Unlike translated anthropic hops, a responses-wire hop KEEPS its normal
  combo position — it is a working member, not a last resort. Direct
  `<ns>/<model>` routes are marked too.
- Error bodies pass through byte-identical (chat and responses share the
  `{"error":{…}}` family).
- **Discovery:** which models belong on the list comes from the ledger's 400
  signatures (`Function tools with reasoning_effort are not supported … use
  /v1/responses`). On 2026-10-12 exactly one model qualified
  (`tokenrouter/openai/gpt-6.1-sol`).
- Verified live 2026-10-12: chat call with tools + `reasoning_effort: high` →
  200 `sol wire ok` via the responses wire, ledger `surface=openai,
  model=openai/gpt-6.1-sol`, after a mock-rig 17/17 pass.
