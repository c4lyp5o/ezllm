# Tool pruning (TokenEconomizer-inspired) — design

Status: **M1 (measurement) implemented by this design.** M2 (prune mode) is
design-locked below and gated on M1's numbers. Inspired by Microsoft Research /
UC Berkeley TokenEconomizer (arXiv 2610.22732): its three levers are tool
pruning (13.2–17.7% cost), micro-skills (11.8–14.3%) and a failure-loop guard
(5.4–7.1%); tool pruning is the biggest and the only one this design covers.

## Why ezllm can host this

Every agent request re-sends the full tool catalog AND the full conversation
history. TokenEconomizer needs an agent plugin to see traces; **ezllm sees the
whole trace for free** (request bodies carry the history), so "which schemas
ship vs which tools actually get called" is derivable server-side with no
plugin, no model call and no response parsing. On SWE-Bench the paper's tool
pruning alone took the first request on task 2 (Django, Claude Code) from
43,351 to 16,295 prompt tokens.

This is complementary to the compress stack, not a competitor to it: the RTK
family (our `rtk`/`tool_ref` engines) compresses tool *outputs*; tool pruning
removes tool *schemas* from the action space. Different body slice, different
policy lifecycle (cross-session learning vs per-message transforms), separate
ledger attribution (the "keep savings classes separate" rule).

## What this is NOT

- Not compression: `tools`, not `messages`; nothing here is a compress engine.
- Not micro-skills (deferred; needs an LLM proposer and per-repo keying a
  router does not have) and not the failure-loop guard (deferred).

## Phases

- **M1 — measurement (shipped by this design).** `tool_pruning.mode:
  off|shadow`. Per (client, tool) usage stats in `tool_usage` (schema v33);
  per-request policy-eligible prunable schema tokens booked to
  `calls.tool_prune_shadow_tokens`; `GET /admin/tool-usage` exposes the
  per-tool prize rollup. **No body is ever rewritten.**
- **M2 — prune mode (design-locked, gated on M1 numbers).** `mode: prune`:
  drop set decided at a session's first request and frozen for the session,
  shape-aware `tools` rewrite, audit window, `tool_choice` protection,
  `calls.tool_prune_tokens_saved` attribution. Go/no-go = the M1 prize rollup
  per client vs real traffic.

## Mechanism (M1)

**Shipped schemas.** `root.tools` entries, both wire shapes: chat
`{type:"function",function:{name,…}}` (name at `function.name`), anthropic
`{name,input_schema,…}` and responses `{type:"function",name,…}` (name at top
level). An entry with no name in either place is UNNAMEABLE and never eligible.
Legacy `functions` is untouched and uncounted. Per-tool size =
`compress.EstimateTokens` over the marshalled entry (the same estimator the
compression floor uses, so the two are internally consistent).

**Used tools.** Walk `messages` (chat/anthropic) and `input` (responses) for
call sites: assistant `tool_calls[].function.name`, legacy
`function_call.name`, content blocks `{type:"tool_use",name}` and responses
`function_call` items; `tool_result` / `function_call_output` / `role:"tool"`
blocks map back through the call ids visible anywhere in the request. A name
seen anywhere counts as used — over-marking used is the safe direction (it can
only keep a tool).

**Stats (`tool_usage`, keyed (client, tool)).** `schema_tokens` (last seen),
`shipped_requests` (+1 per request shipping it), `shipped_sessions` and
`used_sessions` (+1 once per (session, tool) pair), `first_seen_at`,
`last_seen_at`, `last_used_at`. Session identity reuses
`compress.ClientSession(client, X-Ezllm-Session)` with
`compress.ConversationKey(client, body)` as fallback — the exact derivation the
compression commit ledger already uses. The pair-dedupe tracker is
process-local (bounded, TTL'd): a restart can double-count one session per
active conversation, which crosses `min_unused_sessions` at most one session
early — accepted, visible in the counts, never in request behaviour.

**Policy eligibility** (what M2 would drop, what M1 measures):

    used_sessions == 0
    && shipped_sessions >= min_unused_sessions
    && name not in keep
    && name not protected by this request's tool_choice
    && name not called in this request's trace

`tool_choice` naming a tool (`{"type":"function","function":{"name":X}}`,
`{"type":"tool","name":X}`, `{type:"function",name:X}`) protects X;
`tool_choice` demanding *some* call (`required` / `any`) yields an empty
eligible set for that request. `auto` / `none` / absent: normal policy.

Call sites are `tool_use`, `server_tool_use`, `mcp_tool_use`,
`custom_tool_call`, `function_call` and chat `tool_calls`. A tool result whose
call site is not in the body (`previous_response_id`, a compacted history) is
**unresolved**: some tool was used and the router cannot say which. That
request gets an empty eligible set, and its session withdraws every
shipped-but-unused pair it had counted (`shipped_sessions` −1) and stops
counting — a session that hides its calls never pushes a tool toward
eligibility. The withdrawal mark is process-local like the tracker.

**Booking.** `calls.tool_prune_shadow_tokens` = Σ schema tokens of eligible
tools for that request. `mode: off` does nothing at all — no parse, no stats,
no booking.

## Locked DECISIONS

1. **One switch:** `tool_pruning.mode: off|shadow` (M2 adds `prune`). Default
   `off` — nothing changes without an operator action (the ShadowPct
   precedent). `EZLLM_TOOL_PRUNING_MODE` is the env rollback lever (env
   wins). A wrong-typed `mode` / `min_unused_sessions` / `keep` fails boot
   naming the field.
2. **`keep: []string` defaults EMPTY.** Tool names are operator knowledge
   (rtk.go's rule), not guesses baked into the binary. A tool the agent has
   genuinely never used in `min_unused_sessions` sessions is the paper's own
   pruning rule; the residual risk (a never-yet-needed control tool) is
   bounded by M2's audit window and this list.
3. **`min_unused_sessions: 3`** — the paper's "3 consecutive unused tasks".
   The used verdict is **sticky and lifetime**: a tool used in any session is
   never eligible again. Deliberate divergence from the paper's recency
   retirement — a router cannot see why a missing tool broke a run, so it
   errs safe.
4. **Attribution stays per-class:** `calls.tool_prune_shadow_tokens`
   (INTEGER NOT NULL DEFAULT 0) is potential only and never merges with
   `shadow_tokens_saved` (compression) or `context_tokens_*` (budget). M2's
   real saving gets its own sibling column `tool_prune_tokens_saved`.
5. **Shadow measures full policy eligibility**, independent of M2's audit
   window: the measurement must show the steady-state prize; the audit window
   is a wire-time decision only.
6. **Schema v33:** one new TABLE (`tool_usage`, idempotent CREATE via
   schema.sql re-apply) plus one `calls` ALTER
   (`tool_prune_shadow_tokens`), hasColumn-guarded in BOTH `migrate()`
   branches. Migration tests assert the `schemaVersion` constant, fresh DB
   and prior-version-stamped DB both covered.
7. **Write path:** one synchronous multi-row upsert per request carrying the
   deltas (request increments always, session increments and used flips once
   per pair). Precedent: `ReserveClientBudget` already writes synchronously
   pre-dispatch. Steady-state write volume is near zero (pairs are counted
   once per session).
8. **Fail-open everywhere.** A stats write failure or an unparseable body
   changes nothing about the request; measurement can never fail traffic.

## M2 design (locked — implement only on go)

- **Freeze for the cache prefix.** The drop set is decided at the session's
  first request and byte-stable for the session (bounded, TTL'd, process-
  local — the sessionmemo precedent). A name in the set is dropped from
  `tools` on every request of that session; a tool appearing mid-session is
  never in the set. Pruning mid-session would invalidate the upstream
  implicit prefix cache (the 4.6x cold-entry cost we measured on hops).
- **Audit window:** `sha256(session) % 100 < audit_pct` sessions ship the FULL
  catalog (default `audit_pct: 20` ≈ the paper's "every 5 tasks"). A tool
  used during an audit session flips its used verdict and is never dropped
  again — the recovery path for an over-pruned tool.
- **Rewrite placement:** after compression, before `captureShapeDoc` (shape
  capture must measure what actually ships); the shared `jsonbody.BodyDoc` is
  re-opened when bytes change (the prepareHopBody clamp pitfall).
- **Booking:** `calls.tool_prune_tokens_saved` = estimated removed schema
  tokens (real, shipped). Audit/no-op requests book potential to
  `tool_prune_shadow_tokens` exactly as M1 does.

## Runbook (M1)

- **Enable:** `tool_pruning.mode: shadow` in config.yaml, or
  `EZLLM_TOOL_PRUNING_MODE=shadow`, restart. Zero request-behaviour change.
- **Read the prize:** `GET /admin/tool-usage` — per (client, tool):
  `schema_tokens` (per-request cost of the schema), `shipped_requests`,
  `shipped_sessions`, `used_sessions`, `eligible`, `prize_tokens`
  (= schema_tokens × shipped_requests when eligible); `summary.prize_tokens`
  is the go/no-go number. The Requests summary also sums
  `tool_prune_shadow_tokens` per filtered window.
- **Disable:** `mode: off` (or unset); stats rows are kept (they are
  aggregates, harmless) and can be deleted with
  `DELETE FROM tool_usage` if ever wanted.

## Non-goals

Per-repo learning (the router sees clients, not repos — a CWD fingerprint out
of the client's system prompt is a possible later refinement), micro-skills,
the failure-loop guard, and any request-body rewrite in M1.
