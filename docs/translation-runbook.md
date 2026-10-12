# Translation runbook — anthropic clients on openai-only hops

ezllm's native contract is passthrough: bodies leave the box byte-identical to
what the client sent (model swap aside). Translation is the **one deliberate
exception**, and it is double-gated so it can never happen by accident:

1. **Operator knob** — `translation.enabled` in `config.yaml` (default `false`),
   overridden by `EZLLM_TRANSLATION_ENABLED` (flag > env > config > default).
   The effective state is logged at boot: `translation enabled=true|false`.
2. **Proven capability** — the hop's probed `proto_*` facts must say
   `proto_openai = true` AND `proto_anthropic` false-or-NULL. Both flags are
   **NULL until a probe sweep marks them**, so a fresh deploy (or a database
   where the sweep never ran) makes the feature inert even with the knob on.

## What a translated hop does

The client keeps speaking Anthropic (`/v1/messages`, e.g. Claude Code); the hop
is dialed on its native OpenAI `/chat/completions` path with a translated body.
Responses — streaming and not — are translated back into Anthropic shapes.
Ledger rows stay on the **client's** surface (`anthropic`) with the **upstream's**
model. Design and wire contracts: `docs/translation-design.md`.

## Running the probe sweep (this is the rollout gate)

Capability flags come from the dashboard-triggered **ProtocolSweep**
(Models page → protocol sweep), which samples `ProbeMaxModels` (default 6)
models per account × 3 surfaces and writes `proto_openai` / `proto_anthropic` /
`proto_responses` / `proto_tested_at`.

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
  routing). If `opencode-go/step-5-preview-free` matters, make sure the sweep
  covers it.
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

## Responses-wire hops (chat client → /v1/responses hop)

Separate companion feature (`docs/responses-wire-design.md`): some models'
**chat** path refuses tools+reasoning (tokenrouter `gpt-6.1-sol`) while their
Responses path handles both. The curated list is the opt-in:

```yaml
translation:
  enabled: true
  responses_models:
    - "tokenrouter/openai/gpt-6.1-sol"
```

- A marked hop is dialed on `/v1/responses` with a translated body (chat →
  responses), and the response translates back (JSON and SSE). The client
  keeps speaking chat; the ledger row keeps the client's surface and the
  upstream's model.
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
