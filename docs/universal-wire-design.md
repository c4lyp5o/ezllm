# Universal wire matrix — one request, any wire

Design record for the work shipped 2026-10-12/13 (`f7ee36b`, `7294755`, `5a145dd`; local,
not pushed at writing). Source of truth for decisions: the approved plan
`~/workspace/.hermes/plans/2026-10-12_215619-ezllm-universal-wire-matrix-rev1.md`.
Operator knobs: `docs/translation-runbook.md`. This file is the architecture.

## The problem

ezllm speaks three client surfaces (Chat Completions, Anthropic Messages, Responses) and can
reach upstreams on any of those three wires. Before this work only **three** of the six
cross-wire cells existed, each a pairwise translator, and the router could not choose a wire
consistently between a direct route and the same hop inside a combo.

| client ↓ / upstream → | chat | messages | responses |
|---|---|---|---|
| **chat** | native | **M5 added** | existed (curated) |
| **messages** | existed | native | existed (curated) |
| **responses** | **M5 added** | **M5 added** | native |

## The architecture (locked decisions)

1. **Native requests stay opaque.** A request whose client surface matches the upstream wire is
   a byte-stable `model` swap — unknown fields, ordering, everything preserved. The matrix
   never touches it.
2. **Cross-wire routes ride shared models, not pairwise translators.** One request IR
   (`RequestIR`), one response IR (`ResponseIR`), one incremental event model, with **three
   surface codecs** (decode chat/messages/responses, encode chat/messages/responses). Nine
   combinations, three encoders, three decoders — never nine translators chained pairwise.
3. **Explicit history only.** Provider-side Responses history (`previous_response_id`,
   `conversation`, `item_reference`) is refused with a named reason: those references are
   provider-local context and translating them as a fresh conversation would silently lose
   history.
4. **Legacy bytes are a contract.** The three pre-existing cells keep their characterized
   converters (`translate.Request`, `AnthropicToResponses`, `ChatToResponses` and their stream
   readers). The cutover was proved **byte-identical** against a frozen copy of the pre-cutover
   package (`internal/translatelegacy`, extracted from `3a3ab3c`) over 35 bodies × 3 non-stream
   pairs and 35 stream inputs × 3 stream pairs — including truncations, failed terminals, bare
   error frames and undecodable frames. Strict mode (the three new cells) refuses instead of
   guessing.

## Selection (router)

Boot flags: `translation.enabled` (the global translation switch) and
`translation.universal_enabled` (default **false**; `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` is the
no-rebuild rollback). The matrix is **ANDed** with `translation.enabled`: a flag must never
resurrect a layer the operator switched off. A wrong-typed config value fails boot naming the
field.

Per candidate, wire-first:

1. **Explicit intent first.** `translation.responses_models` wins (operator naming a wire);
   under universal an explicit model **pin** names the wire too, honored before the native rule.
   A conflicting override and pin is a typed refusal.
2. **Native when possible or unproven-impossible.** Proven-true OR NULL (untested) stays native
   — NULL is "never probed", not "impossible".
3. **Alternatives only on proof.** Only a PROVEN-impossible native wire opens an alternative,
   and the destination must be PROVEN capable (NULL or a lookup error is not translation
   evidence). Fixed preference:
   - Messages client → chat, then responses
   - chat client → responses, then messages
   - Responses client → chat, then messages
4. **A Messages destination needs the operator's fact.** `accounts.usage_convention`
   (`auto|anthropic|total`, schema v13) states how the upstream's own `input_tokens` counts
   cache tokens. `auto` = unstated → the cell is skipped (the refusal names the field), never
   guessed. See `docs/usage-convention-design.md`.

Pin semantics: with universal **off**, a pin to another wire is a hard refusal (unchanged). With
universal **on**, the pin is intent — but a pin onto a wire the probe disproved is refused, and a
Messages pin still requires the convention.

## Conversions (proxy)

The pair `(client surface, upstream wire)` decides the codec path in one shared predicate
(`translate.IsUniversalCell`), so routing and encoding cannot disagree. The three new cells ride
the strict codecs; the legacy cells keep their converters. A cross-wire pair with no converter
refuses — it never falls through to the model swap.

- **Requests**: decode the client's shape, encode the upstream's. Unsupported features are typed
  refusals (media, reasoning replay, non-function tools, `allowed_tools`, `text.format`,
  `include`, `background`, `top_k`, Messages reasoning effort, unknown fields) with a reason
  each. A refusal happens **before any upstream call** (zero spend) and never blind-fails-over —
  a 400 is the request's fault, not the hop's.
- **Responses**: upstream error envelopes are re-shaped into the client's error family (chat and
  responses share `{"error":{…}}`; messages wraps it as `{"type":"error","error":{…}}`).
  Unparsable bodies pass through byte-identical.
- **Streams**: one decoder per source wire, one encoder per destination, bounded SSE framing
  (comments, CRLF, multiline data, EOF-tail compatibility). Upstream item id, public tool call
  id, source index and client encoder index stay distinct; argument fragments stay strings until
  the block closes. Strict mode verifies the terminal output against the delivered deltas and
  refuses a contradiction.

## Accounting and outcomes

- `tokens_in` stays inclusive for translated rows (the documented exception): the upstream's own
  total passes through, cache counted inside, `cread` 0. The `translated` marker says which rows
  these are. No new markers were added.
- Partial usage snapshots **merge, never sum** (a `message_delta.output_tokens` is cumulative).
- `Result.StreamOutcome` records how a translated stream actually ended — `completed`, `failed`
  (a late error frame), `truncated` (the source ended without a terminal event) — and the ledger
  row carries it in its error column even though the delivered status stays 200 (that is what
  the client saw). The positive hop signal (`MarkHopGood`) is withheld unless the stream
  completed.
- **Known boundary**: translated candidates ride the tail of a combo, outside strategy ordering
  (a deliberate pre-existing rule: "a native hop always wins"). The withheld hop signal therefore
  has no routing effect for translated hops today — it is only visible in the ledger. Letting
  universal hops participate in strict/sticky rotation is a routing-policy change and was
  deliberately not made.

## Compatibility

- Universal **off** keeps every behaviour byte-for-byte: the single messages→chat alternative,
  the curated responses-wire list as its own opt-in, pins as hard refusals.
- The three legacy cells keep their converters and their exact bytes.
- No production config, account curation or capability verdict is changed by enabling the flag;
  nothing is auto-probed.

## Verification (all local, `go test ./... -count=1` green at each milestone tree)

- M3 parity harness against the frozen pre-cutover package (byte-identical; see above).
- 9 cells × stream/nonstream asserting the **actual** upstream path and body and the client's
  return shape; every diagonal asserts native byte identity (unknown fields included).
- Tool loops (call → result replay → new call with its public identity) through all six
  cross-wire cells; 429 mixed-wire fallback; 400 no blind failover; late stream errors; zero-spend
  preparation refusals.
- Sensitivity bites at every milestone (usage merge, tool identity, truncation synthesis,
  universal gate, convention guard, cell predicate, outcome honesty) — each fell and restored
  byte-exact. A bite that did NOT fall exposed a coverage gap (`call_id`-quoted argument
  fragments), which was closed with `TestStreamPortableToolIdentityByCallID` before re-biting.
- `-race` clean on router/proxy/server/store.

## Not proven yet

Static/mock green is not provider compatibility. Live canaries (scratch instance, a real tool
loop on each newly supported cell, then a repeated-prefix request to observe cache behaviour)
need Calypso's approval and real token spend.
