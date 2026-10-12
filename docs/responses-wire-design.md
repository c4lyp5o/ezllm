# Responses-Wire Hop Translation (chat client → /v1/responses hop)

> **2026-10-13:** the universal wire matrix (`docs/universal-wire-design.md`) supersedes this
> doc's exclusions for the opt-in `translation.universal_enabled` mode: a Responses hop no
> longer needs this curated list to be selected (proven capability + the fallback chains
> suffice), and three more cross-wire cells exist. With the flag **off**, everything here
> still stands exactly as written.

> Companion to `docs/translation-design.md` (anthropic→openai). Same rules:
> golden fixtures are the spec, byte-determinism is the invariant, the
> operator decides which hops ride the override.

**Goal:** let a combo hop that only behaves on OpenAI's **Responses API** serve
clients that speak **chat completions** — starting with
`tokenrouter/openai/gpt-6.1-sol`, whose chat path refuses `tools +
reasoning_effort` (`400: Function tools with reasoning_effort are not
supported for gpt-6.1-sol in /v1/chat/completions`). The client keeps its wire;
the HOP is dialed on `/v1/responses` with a translated body and the response is
translated back. That makes sol a first-class member of any chat combo (e.g.
`daily`) with tools AND reasoning, and a hop failure still fails over
normally (the 400 class only fires upstream if the model itself misbehaves).

**How a hop is marked:** the operator-curated list
`translation.responses_models: ["<namespace>/<model_id>", …]` in config.yaml,
gated by the same master switch `translation.enabled`. Each entry is a **regex
matched fully against `<namespace>/<model_id>`** (both ends anchored), so a
family pattern like `space-stationine/gpt-6\.1-sol` covers its variants while a
plain id matches only itself; an uncompilable entry fails the boot by name.
Empty list (default) = feature inert. Ledger evidence is the discovery
mechanism: a hop whose chat calls 400 with the tools/reasoning signature is a
candidate — and marking a hop that does NOT need it costs native fidelity, so
patterns are scoped per account rather than globbed across the fleet.

**Invariants carried over:** passthrough identity for unmarked hops (tested),
byte-determinism (ordered structs, golden byte-exact tests), the route keeps
the CLIENT's surface (ledger + usage tap never see the translation), upstream
error bodies pass through byte-identical (chat and responses share the
`{"error":{…}}` family — no error translation needed), and a marked hop keeps
its normal combo position (this is a wire quirk, not a last-resort hop).

---

## Request: openai chat → openai responses

| Chat field | Responses field | Notes |
|---|---|---|
| `messages[role=system]` (first, string) | `instructions` (top-level string) | only the first system message; later ones are dropped (documented) |
| user message, `content` string | `input[] {role:"user", content:"<string>"}` | |
| assistant message, `content` string (may be null with tool_calls) | `input[] {role:"assistant", content:"<string>"}` — omitted when empty | |
| assistant `tool_calls[{id, function:{name, arguments}}]` | one `input[] {type:"function_call", call_id:"<id>", name, arguments}` per tool call, AFTER the assistant text item | arguments stays a string, verbatim |
| tool role message `{tool_call_id, content}` | `input[] {type:"function_call_output", call_id:"<tool_call_id>", output:"<content>"}` | content may be string or blocks → flatten to text |
| `tools[{type:"function", function:{name, description, parameters}}]` | `tools[{type:"function", name, description, parameters}]` | flatten one level |
| `tool_choice:"auto"|"none"|"required"` | same string | |
| `tool_choice:{type:"function", function:{name}}` | `{type:"function", name}` | |
| `max_tokens` | `max_output_tokens` | |
| `temperature`, `top_p` | same, omitted when absent | |
| `stop` | dropped | no responses equivalent (comment in code) |
| `stream` | `stream` | omitted when false (omitempty) |
| `stream_options` | dropped | include_usage does not exist on responses |
| `reasoning_effort:"low|medium|high"` | `reasoning:{effort:"…"}` | only when present |
| `parallel_tool_calls` | same, omitted when absent | |
| `model` | the hop's upstream model id | replaces the swap, like anthropic→openai |
| unknown fields | dropped silently | forward-compat, same as anthropic→openai |

**Input item order:** the `input` array preserves the conversation order of
the source messages, with each assistant tool_calls message expanding to its
text item (if any) followed by its function_call items.

## Response: openai responses → openai chat (non-streaming)

| Responses | Chat |
|---|---|
| `id` | `id` |
| `created_at` (unix) | `created` |
| `output[]` items `type:"message"` → `content[] type:"output_text"` | concatenate → `choices[0].message.content` |
| `output[]` items `type:"function_call"` (`call_id`,`name`,`arguments`) | `choices[0].message.tool_calls[] {id:call_id, type:"function", function:{name, arguments}}` (after text) |
| `status:"completed"` with ≥1 function_call item | `finish_reason:"tool_calls"` |
| `status:"completed"` otherwise | `finish_reason:"stop"` |
| `status:"incomplete"` (incomplete_details.reason `max_output_tokens`) | `finish_reason:"length"` |
| `usage.input_tokens` / `usage.output_tokens` | `usage.prompt_tokens` / `usage.completion_tokens` |
| — | `object:"chat.completion"`, `model:<upstream model>` |
| `output[]` reasoning items (`type:"reasoning"`) | dropped silently | Claude-Code-shaped reasoning echo is not a chat field |

## Response: responses SSE events → chat chunks

Upstream events (stream:true) and what the client must receive, in order:

1. `response.created` `{response:{id, model}}` → first chunk
   `{"id":…,"object":"chat.completion.chunk","created":…,"model":<client model>,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
2. `response.output_text.delta` `{item_id, delta}` → content chunk
   `choices[0].delta.content = delta` (no role field after the first)
3. `response.output_item.added` with `item.type:"function_call"` (`call_id`,`name`) → tool_call OPEN:
   `choices[0].delta.tool_calls:[{index:<per-call counter>, id:call_id, type:"function", function:{name, arguments:""}}]`
   The index is assigned per call in arrival order (0,1,2…).
4. `response.function_call_arguments.delta` `{item_id, delta}` → fragment:
   `choices[0].delta.tool_calls:[{index:<that call>, function:{arguments:delta}}]`
   Plain string concat per call — fragments land at arbitrary boundaries.
5. `response.completed` `{response:{status, usage}}` → the closing pair, exactly
   OpenAI's `include_usage` shape:
   - `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"|"tool_calls"|"length"}]}` (usage:null)
   - then `{"choices":[],"usage":{"prompt_tokens":…,"completion_tokens":…}}`
   - then `data: [DONE]`
6. `response.failed` / `response.incomplete` with an error → emit the upstream
   error object verbatim as one `data:` frame, then `[DONE]` — chat and
   responses error bodies share the `{"error":{…}}` family. Never a bare close.
7. Any other event type (reasoning summaries, refusals, web search) → skipped,
   never forwarded, never fatal.

**CRLF discipline:** frames are `data: <json>\n\n` (chat SSE has no `event:`
lines). Golden fixtures pin the bytes.

## Design decisions

- **New functions in `internal/translate`:** `responses_request.go`
  (`ChatToResponses`), `responses_response.go` (`ResponsesToChat`),
  `responses_sse.go` (`NewResponsesSSEReader(r, clientModel)`). Same package,
  same ordered-struct discipline, `testdata/` golden pairs.
- **Route marking:** `Route.ResponsesWire bool`, set by the router when the hop
  matches the curated list and the client surface is openai. The route keeps
  `Surface = openai`; `attempt()` dials `SurfacePath(SurfaceResponses)`.
- **prepareHopBody:** `ResponsesWire` → `translate.ChatToResponses(body,
  rt.Model)` (replaces the model swap, same as translated hops).
- **commit:** stream → wrap `resp.Body` with `NewResponsesSSEReader` (Close
  still reaches the original body); non-stream → `ResponsesToChat(raw,
  rt.Alias)`; error statuses (≥400) pass through byte-identical — no error
  translation on this wire. An untranslatable success body is a 502 (never
  responses-shaped bytes to a chat client).
- **Ordering:** a ResponsesWire hop keeps its normal combo position — it is a
  working member, not a last resort. No tail rule (unlike translated hops).
- **Config:** `translation.responses_models: ["tokenrouter/openai/gpt-6.1-sol"]`
  + validation (non-empty entries, must contain `/`). Master switch
  `translation.enabled` gates it together with the anthropic→openai feature.

## Not building (YAGNI)

- Responses→chat direction for clients (no client needs it).
- Auto-marking hops from 400 signatures (follow-up candidate; the ledger scan
  is the discovery mechanism today).
- A `wire` sweep probe carrying tools+reasoning (the minimal probe can't see
  this limitation; revisit if curation becomes tedious).
- Images/multipart input beyond string and simple text blocks.
