# Services — per-service default models (design, M10)

Status: **sign-off ready**. Owner: Melissa (with Calypso). Supersedes: nothing.
Related: `docs/API.md` (contract), `web/src/pages/Chat.tsx` (combobox pattern),
`internal/server/admin.go` `handleComboTest` (probe-through-dispatcher pattern).

## Problem

`POST /v1/embeddings`, `/v1/images/generations`, `/v1/audio/speech`,
`/v1/audio/transcriptions` are routed like every other surface: the request must
name a model (`ns/model` or a combo name) or it is refused `400 'model' is
required`. A first-time user who just connected a provider has no idea which of
its models serves which service, and every client must be taught an exact
namespaced id. The operator wants a **Services** page: one card per service, a
picker for the model that service should use, and honest empty states when
nothing is configured ("those routes should not respond").

Two facts constrain the design:

1. Upstream catalogs do **not** advertise per-service capability. The existing
   probe verdicts (`models.proto_*`) cover chat wires only (openai /
   anthropic / responses). Nothing in the data says "this model embeds".
2. A probe for these services can cost real money (image generation), so no
   sweep may ever run one unattended.

## Shape of the solution

A **service binding** is the DEFAULT model for one service endpoint, used when
the client's request omits `model`. An explicit `model` in the request always
wins — the binding is a default, not a routing override. (Label it "Default
model for /v1/embeddings" in the UI; nothing grander.)

Because the bound value is fed to `ResolveCandidates` as the `requested` string,
a binding may be either `<namespace>/<model_id>` (direct route) or a bare combo
name (failover chain). The UI lists models only in this milestone; the backend
accepts whatever the resolver accepts.

### Verified vs selected

A model's fit for a service is only ever **proven by a real request**. A per-card
`Test` action runs one tiny, endpoint-appropriate real request through the real
dispatcher (same path as `POST /admin/combos/{id}/test`: `ResolveCandidates` →
`ForwardCandidates`; not ledgered) and records a verdict per
`(surface, account, model)`. Tests are strictly opt-in — the operator clicks,
nothing sweeps.

- `embeddings` → `{"model":M,"input":"ping"}`
- `images_gen` → `{"model":M,"prompt":"a small red circle","n":1}` (**costs
  money** — the UI confirms before sending)
- `audio_tts` → `{"model":M,"input":"ok","voice":"alloy"}` (`alloy` is OpenAI's
  voice vocabulary; a provider with its own vocabulary answers with its own
  words, which the verdict keeps verbatim)
- `audio_stt` → multipart: `model` field + a ~1 KB silent WAV (0.2 s, 8 kHz,
  16-bit mono); Whisper-class models answer 200 with empty text

A green test proves **the wire answers this service shape** — not output
quality. The UI says so.

### Binding modes

- `manual` — the picked model serves the endpoint (works unverified; the card
  shows `not yet tested`).
- `auto` — the most recently **tested-ok** model for the surface serves the
  endpoint. With no tested-ok model the route refuses (`service_not_configured`,
  wording: auto has no tested model yet).
- cleared (`off`, no row) — requests must name their own model.

## Locked DECISIONS

1. **Binding = default for requests that OMIT `model`.** Explicit `model` in the
   request always wins; chat surfaces (`chat`/`anthropic`/`responses`) are
   unaffected and keep requiring `model`. Applies to the four service surfaces
   only (`embeddings`, `images_gen`, `audio_tts`, `audio_stt`).
2. **Schema v15, two tables.** `service_bindings(surface PRIMARY KEY, mode,
   model_target, updated_at)` — `mode` ∈ `manual|auto`, validated in Go (no
   CHECK: SQLite cannot alter a constraint; same rule as `combos.strategy`).
   `service_probes(surface, account_id, model_id, ok, detail, tested_at,
   PRIMARY KEY(surface, account_id, model_id))`, FK accounts ON DELETE CASCADE.
   No ALTERs needed — `schema.sql` re-apply creates both tables in both migrate
   branches.
3. **Refusal vocabulary.** Unconfigured service + omitted `model` →
   `400 {"type":"service_not_configured", extra:{service, configure:"/services"}}`
   — distinct from `model_not_found` (a named model nobody serves) and from the
   plain `'model' is required` (chat surfaces). The refusal is ledgered like
   every other resolve failure. First-time semantics: no providers → the cards
   read "not available" and every path refuses clearly.
4. **`auto` = most recently tested-ok** (`tested_at DESC`, then account_id,
   model_id for determinism) whose catalog entry still exists. Never an
   unverified guess. `auto` with zero tested models refuses per decision 3.
5. **Tests run through the REAL path** — `ResolveCandidates` (so pins and
   capability gates refuse the test exactly as they would refuse traffic) then
   `ForwardCandidates`; verdict stored per `(surface, account_id, model_id)` of
   the SERVED route; not ledgered. Test bodies as listed above;
   `images_gen` is the only card that confirms before sending (real cost).
6. **Target validity is visible.** The binding's target is validated on write
   (namespace must resolve, model must be in that account's catalog) and
   re-checked on read (`target_exists`) so a resync that dropped the model shows
   `bound model missing` instead of failing at request time.
7. **Multipart STT defaulting needs model-field INJECTION.** `swapModelMultipart`
   today errors when the body has no `model` field; it now APPENDS one
   (`rt.Model`) instead. JSON needed no change (`SwapModel` already assigns the
   key). A missing-model multipart with no binding is refused before dispatch,
   so injection only ever happens for a resolved default.
8. **`extractModel`/`extractModelMultipart` stop erroring on an absent/empty
   model** — they return `""`; the HANDLER decides per surface (chat: `'model'
   is required` 400, unchanged wording; service: binding resolution). Non-JSON
   bodies still 400.
9. **Admin API** (docs/API.md §M10):
   - `GET /admin/services` → `{services:[{surface, endpoint, label,
     description, mode, model_target, resolved_target, target_exists,
     verdict:{ok,detail,tested_at,model}|null, verified_models, accounts}]}` —
     one row per service surface (always all four; `mode:"off"` when unbound).
   - `PUT /admin/services/{surface}` `{"mode":"manual|auto|off","model":"…"}`
     (strict decode; `off` clears the row; `manual` requires a resolvable
     model; `auto` ignores `model`). → 200 echoes the row.
   - `POST /admin/services/{surface}/test` `{"model":"ns/model"}` → 200
     `{surface,model,ok,status,ms,detail}` and writes the verdict. Unknown
     surface 400; unresolvable model 404.
10. **UI**: sidebar `Services` (hint `embeddings · images · audio`) after
    Providers; `PAGE_HEADERS` entry; four cards; searchable combobox (moved from
    `Chat.tsx` into `ui.tsx`, Chat re-imports — no duplication) whose options are
    `Not configured`, `Auto — first tested model`, then every catalog model
    (`ns/model`, same flattening Chat uses). First-run explainer line under the
    page header. Empty state: whole card `not available — connect a provider
    first` + CTA to `/providers`. Status is TEXT per the standing rule.
11. **Ledger**: a defaulted request records `alias`/`model` as the resolved
    target — the same contract as a named request. No new ledger columns.

## Non-goals (this milestone)

- Capability filtering of the picker (no catalog data supports it; the combobox
  search is the discovery aid).
- Auto-routing across providers, weights, or cost — that is what combos are for.
- Chat-surface bindings.
- Sweep/batch service probing (image probes cost money; per-card opt-in only).

## Addendum 2026-10-21 — five more service surfaces

`images_edit` (`/v1/images/edits`, multipart like STT — shares the multipart handler),
`moderations`, `rerank`, `videos_gen`, `music_gen` join `ServiceSurfaceNames`, the pin
vocabulary and the ledger surfaces. Probes: images_edit sends a 1x1 PNG + prompt (multipart),
moderations `{model,input}`, rerank `{model,query,documents:["ping","pong"]}`, videos/music
`{model,prompt}`; images_edit, videos and music are confirmed in the UI before each test (paid).
Video and music have no common upstream wire: ezllm passes the body through, so "OpenAI-style"
means whatever the bound provider accepts.

Fix shipped alongside: `ModelFacts.Supports` / `ModelSupportsSurface` mapped every service
surface to the CHAT probe column (`proto_chat`), so a model whose chat probe failed — every
embedding/image/rerank model — was refused on its own endpoint. Service surfaces now read as
unknown (NULL) there, and `SetModelProto` ignores them.

## Addendum 2026-10-21 — "Try it" (generation from the card)

Media services (`images_gen`, `images_edit`, `audio_tts`, `audio_stt`, `videos_gen`,
`music_gen`) get a generator in their card; embeddings, moderations and rerank do not (their
output is numbers/labels — Test is enough). It requires a SAVED default: the request carries no
model, so the binding serves it and the generation is exactly what a model-less client gets.
Backend: `POST /admin/services/{surface}/generate` → `s.inference(surface)` behind admin auth
(the `/admin/chat` pattern) — no new dispatch code, full output, a ledger row. Kept separate
from Test: a generation never writes a service verdict. Output rendering walks the JSON for
playable media (`web/src/utils/media.ts`): data URIs, URLs with a media extension or under a
media-named key (`url`, `video_url`, …), long base64 typed by the surface; a `status_url` /
job id is shown as a link, not played. Every run is confirmed first (it spends quota).
