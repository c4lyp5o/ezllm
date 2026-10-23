# Universal wire matrix — one request, any wire

Design record for the work shipped 2026-10-12/13 (`f7ee36b`, `7294755`, `5a145dd`).
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

Boot flags: `translation.universal_enabled` (default **false**;
`EZLLM_TRANSLATION_UNIVERSAL_ENABLED` is the no-rebuild rollback) — the **single switch for the
matrix**. It does not require `translation.enabled` (operator decision 2026-10-13): with the
flag on, the whole matrix is available through one knob. With it off, nothing changes
byte-for-byte. `translation.enabled` remains the legacy gate for the single anthropic→chat
alternative when universal is off. A wrong-typed config value fails boot naming the field.

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
4. **A Messages destination reads the operator's fact, and `auto` is not a gate.**
   `accounts.usage_convention` (`auto|anthropic|total`, schema v13) states how the upstream's
   own `input_tokens` counts cache tokens. `auto` reads as exclusive — the meaning the ledger has
   always given it (usage-convention-design D1) — so a chat or Responses client reaches an
   Anthropic hop the way Claude Code reaches an OpenAI one. (Revised 2026-10-20: the earlier
   refusal turned every chat client away from every unconfigured Anthropic account.) An
   `anthropic-compatible` account's Messages wire counts as proven by the kind itself when its
   probe never ran; a probe that disproved it still wins.

Pin semantics: with universal **off**, a pin to another wire is a hard refusal (unchanged). With
universal **on**, the pin is intent — but a pin onto a wire the probe disproved is refused.

## Conversions (proxy)

The pair `(client surface, upstream wire)` decides the codec path in one shared predicate
(`translate.IsUniversalCell`), so routing and encoding cannot disagree. The three new cells ride
the strict codecs; the legacy cells keep their converters. A cross-wire pair with no converter
refuses — it never falls through to the model swap.

- **Requests**: decode the client's shape, encode the upstream's. Only a feature whose loss would
  change what the client gets back is a typed refusal: media the target cannot carry,
  non-function tools, `allowed_tools`, a constrained output format (`response_format` /
  `text.format` other than text), legacy `functions`, audio output, provider-side state
  (`previous_response_id`, `conversation`, `item_reference`, `background`), `top_k`. A refusal
  happens **before any upstream call** (zero spend); the candidate loop then skips that hop like
  a cooling one and the refusal is the answer only when no later hop serves.
- **Dropped, as the legacy converters drop them** (revised 2026-10-20): unknown top-level
  fields (vendor extensions such as `prompt_cache_key`, `safety_identifier`, `verbosity`),
  `include`, `reasoning.summary`, replayed Responses reasoning items (provider-private, often
  encrypted), and `reasoning_effort` on the Messages wire (the upstream's default effort
  applies; chat and Responses still carry it).
- **Messages encoding fills what the wire requires**: `max_tokens` defaults to 32000 when the
  client sent none; a tool without a schema gets `{"type":"object","properties":{}}`; tool-call
  ids outside `[A-Za-z0-9_-]` are rewritten byte-wise (`functions.read:0` → `functions_read_0`,
  the same on the `tool_use` and its `tool_result`). When distinct IDs sanitize to the same
  spelling, the encoder appends a deterministic hash suffix to disambiguate them, reserving
  existing legal IDs before assigning suffixes. Empty turns are dropped and consecutive
  same-role turns merged; `parallel_tool_calls:false` becomes `disable_parallel_tool_use`.
- **Function tools retain explicit `strict` settings** across Chat, Responses, and Messages;
  absent and false remain distinct. Parameter schema bytes remain unchanged.
- **Messages encoding follows the destination model** (`translate/claude_models.go`, matched on
  the Claude id wherever the provider puts it — `anthropic/claude-sonnet-5.5`,
  `us.anthropic.claude-opus-4-8`, `claude-haiku-5-5[1m]`; any other id is untouched):
  `temperature`/`top_p` are dropped for Opus 4.7+, every 5.x model, Fable and Mythos (they
  reject them); a forced `tool_choice` on Fable 5.1, Mythos 5.1, Opus 5.5 and Sonnet 5.5 (which
  reject forcing) becomes `auto` plus a one-line instruction appended after the cache
  breakpoints, so the cached prefix is unchanged; and a model that thinks by default (5.x, Fable,
  Mythos) gets `thinking: {type: adaptive, display: summarized}` — visibility only, same
  thinking and billing — unless a forced choice remains, where an explicit thinking parameter
  would be refused.
- **Reasoning reaches chat clients**: a Messages `thinking` block (stream or not) becomes
  `reasoning_content` on the chat wire, never part of `content`. The Responses and Messages
  encoders drop it; it is never sent back upstream.
- **Prompt caching crosses onto Messages**: the client's own `cache_control` markers (content
  parts, or a message-level marker) are carried, capped at four (the first plus the newest
  three). A body with none gets the standard two — end of system, end of the newest message —
  placed as a pure function of the body, so the prefix stays byte-stable across turns.
- **Responses**: upstream error envelopes are re-shaped into the client's error family (chat and
  responses share `{"error":{…}}`; messages wraps it as `{"type":"error","error":{…}}`).
  Unparsable bodies pass through byte-identical.
- **Streams**: one decoder per source wire, one encoder per destination, bounded SSE framing
  (comments, CRLF, multiline data, EOF-tail compatibility). Upstream item id, public tool call
  id, source index and client encoder index stay distinct; argument fragments stay strings until
  the block closes. Strict mode verifies the terminal output against the delivered deltas and
  refuses a contradiction. A Messages source's non-portable blocks — `redacted_thinking`,
  server-tool use and results — are consumed whole, deltas and stop included (`thinking` is
  carried as reasoning, above), and annotation deltas (`citations_delta`, `signature_delta`) are ignored;
  `model_context_window_exceeded` is a length stop and `pause_turn` a completed turn. A truly
  unknown stop reason still fails.

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
- **Translated hops are ordinary citizens (operator decision 2026-10-13).** A translated hop is
  ordered, held good and rotated on exactly the same terms as a native one: a successful
  translated hop IS a success, a failed one IS a failure, and the positive hop signal is
  withheld unless its stream completed. An earlier design parked translated candidates outside
  the strategy ordering ("a native hop always wins"); that was reversed, so a mixed combo can
  now dispatch on a translated hop by strategy choice.

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
