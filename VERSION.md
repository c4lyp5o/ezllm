# VERSION

Version history for ezllm. From v1.0.0 the scheme is semantic versioning:
MAJOR for a breaking change to the API, config or data, MINOR for a feature,
PATCH for fixes. A release is a git tag `vX.Y.Z` that matches this file
(`release.yml` and `docker-publish.yml` refuse a tag that does not). Before
1.0.0 the scheme was one version per change date (`v0.0.1` … `v0.0.21`).

The current version is served by `GET /healthz` (`version` field), shown in the
dashboard sidebar, and asserted against this file by
`internal/server/version_test.go` — bump `internal/server/version.go` and this
file in the same commit, or the test fails.

**Current: v1.4.0**

## v1.4.0 — 2026-10-28

One feature and a batch of fixes. No API, config or schema change; no
migration. MINOR because a feature shipped, not because anything broke.

**The Neighbourhood is a city.** The page that used to draw a row of houses now
draws a generated isometric city on one canvas: one building per provider
account on a seeded street grid, so the same providers always grow the same
town and a building never moves when traffic does. Height and window light are
tokens per minute over the last five minutes (log scale, full glow = the
busiest provider, never under 2,000 a minute); rain falls where a quarter or
more of recent calls failed; boarded windows mean the account or all of its
keys are disabled; the number of cars on the street follows the live request
rate of the last minute. Missing usage is drawn as `unknown` and never as a
quiet zero. A committed row on `/admin/stream` flares that provider — the light
climbs the facade from the pavement — and sends a taxi to its door: the feed is
a real per-request source, so every flare is a call that actually happened.
Everything else on screen is scenery and carries no number: people on the
pavements (with umbrellas in the rain), cars with cabins, glass, wheels and
lamps, a plane crossing behind the towers every minute or two, occasional
showers that can also be commanded from a new Auto sky / Rain / Clear control,
and at night the windows of a lit building smeared down the street in front of
it. The provider table gained a trend arrow for what moved since the last
update.

Two scene bugs were found while building it and are fixed here: the animation
clock never advanced, so traffic lights never changed, rain streaks hung frozen
mid-air and nobody walked; and a single request arriving before the first frame
stopped the city from ever seeding its people. Neither showed in a screenshot.

The city is cheap by construction — Canvas 2D, never WebGL, one affine matrix
per building face and one depth-sorted pass for occlusion. Measured 60 fps at
1150x598 in clear sky, 56 fps in rain, 60 fps at 390x844; device pixel ratio
capped at 2, the loop stops when the tab is hidden or the scene is paused, and
reduced motion renders one still frame carrying the same figures. Every
building is a real `<button>` clipped to its own silhouette, stacked in depth
order like the city, so keyboard and screen readers reach each provider and the
table and inspector carry the exact unabbreviated numbers.

**Provider keys belong to their account.** A retest or an enable/disable now
checks the key against the account in the URL and answers 404 before any
provider probe or key update. The dashboard batch alongside it is mostly
honesty work: report and quota load failures show an unavailable state and
retry instead of implying zero usage, cost or caps; the overview exposes failed
ledger flushes; login and first-boot setup count in-flight attempts; chat
cancels in-flight requests when you leave the page; and model pickers, dialogs
and the mobile comparison view were made usable with a keyboard and a screen
reader. The last two releases' cancellation work is not repeated here — see
v1.3.2 and v1.3.3 below.

## v1.3.3 — 2026-10-27

One fix, found by a test that had been failing about one run in five without
anyone noticing. No API, config or schema change; no migration.

**A cancelled caller now gets the cancellation, not a parked hop error.** v1.3.2
stopped a client hang-up from charging a strike against the hop, but the request
could still end as a *success* carrying a hop's 503. When the last hop answers a
failover status its response is parked, to be committed if nothing else serves.
The loop's context check then broke out without clearing that parked response,
so `commit` wrote the 503 and returned a nil error: the cancellation reached
neither the server nor the ledger, and the hop's error was reported as the reason
the call ended. Which branch you got depended on the race — a hop whose transport
noticed the cancel surfaced `context.Canceled` (~82% of runs), a hop that had
already written its 503 was committed as a success (~18%).

`TestClientCancelStopsTheFailoverLoop` measured 9 failures in 50 local runs
before the change and 0 in 200 after. The fix is one branch at the loop head: a
cancellation outranks a parked fallback, so the body is discarded and the error
path answers, preserving any earlier transport error as the first cause.

Two things also landed that are not runtime changes: `docs/install-binary.md`,
the manual for the released Linux/macOS/Windows binaries, and the CI rule that
`:latest` is master-only so a tag build can never roll it backwards.

## v1.3.2 — 2026-10-27

Five fixes, three of them found by reading the gateway's own ledger rather than
by review. No API, config or schema change; no migration.

**The reasoning-effort clamp now actually runs.** It shipped in v1.2.0 and was
inert on every request: the resolver reads hop facts from a catalog SNAPSHOT, and
the snapshot's model query never selected `reasoning_vocab` / `reasoning_required`,
so the rule was always empty while the direct row read had it — which is why the
tests were green and production kept refusing the same values. It also could not
serve a model that requires reasoning: the vocabulary and the mandate arrive as
separate facts, so the clamp offered `off` to a model that refuses it. `Required`
now governs the selection and the field is dropped when nothing at or below the
ask is legal. 20 of the 29 `bad_request` 400s in the week before this release were
this one bug.

**A client hanging up no longer parks the hop.** A cancelled caller context was
classified as a transport error, so the hop banked a strike and the default
threshold parked it for five minutes — closing a tab took a healthy endpoint out
of rotation. All 80 of the 502s in the week before this release were
`context canceled`. A cancellation is now a third outcome: no strike, no park, no
cleared park, a `client cancelled` trail note at Info, and the ledger row records
`499` instead of `502 upstream_error`.

**github-copilot: the persona stealth rule runs, and claude/gemini are no longer
dialled on the responses wire.** Both rules existed only in functions the linker
had already pruned, so SOUL/MEMORY/USER have been reaching a web provider. The
stealth rule is a deliberate privacy behaviour with real output loss: a request
whose only content was a system prompt now leaves with an empty `messages` list.
claude*/gemini* are refused before the dial with a typed hop fact and the
dispatcher fails over instead of committing a mystery 400.

**The error table stops calling a bad body "capacity".** opencode's edge wraps
request-shape faults in an `Upstream request failed:` envelope typed
`server_error`; the envelope won, so malformed requests were counted as provider
weather and the dashboard told the operator to wait. The bracketed cause inside
the envelope now wins.

**A history refusal fails over instead of ending the run.** A thinking model that
demands the reasoning of every earlier assistant turn cannot serve a conversation
another model started; it answers a 400, and 400 is not in the failover set, so
the run died where another hop of the same family could have served it. That
class is now recognised (`[reasoning_echo_required]`), it fails over, and it does
NOT park the hop — the same model is healthy for conversations it started. The
400 still reaches the client if every hop refuses.

Every fix carries a test at the seam that broke, and each was bitten against its
own producer before commit. The clamp fix was additionally proved end-to-end on a
canary rig against the real provider.

## v1.3.1 — 2026-10-26

Settings administration: persisted runtime policy with source/override visibility,
local display preferences, live gateway controls, session management, notification
preferences, sanitized diagnostics, and verified encrypted scheduled backups with
offline recovery. YAML/environment deployment configuration remains supported.
Schema v31 adds operator settings, job-health and session metadata. Existing empty
model-rule timezones migrate to UTC; new rules can inherit the instance timezone.
Export format version 1 remains compatible with an additive settings field.
Back up the database and active key before upgrading; use the pre-upgrade pair
with the previous binary for rollback.

Provider presets now read Google Antigravity, Claude Pro/Max, ChatGPT Plus/Pro,
and OpenAI Platform. Provider IDs, namespaces and existing account names are
unchanged. The dashboard includes the rebuilt assets for these labels.

---

## v1.3.0 — 2026-10-26

Breaking release; one non-additive schema step (v29 -> v30). The migration
renames `models.proto_openai` to `proto_chat` and rewrites stored pins and
ledger surfaces from `openai` to `chat` in place. A v29 binary refuses a v30
database, so roll back from a backup taken before the upgrade.

- **The chat-completions wire is named `chat`, not `openai`.** Ledger
  `surface`, model pins, `protocol_support`, the `/admin/models` capability
  keys and the dashboard all use `chat`; scripts reading `openai` from the API
  or the ledger must switch. `openai` is still accepted as input for model pins
  and the combo-test surface. Provider kinds (`openai-compatible`,
  `gemini-openai`) are unchanged.
- **Probes are no longer ledgered.** Key tests, protocol sweeps, combo tests and
  service tests leave no ledger row; their verdicts are still stored. This
  reverses the v1.2.0 rule that failed probes write their reason into the
  ledger `err`.
- **Claude subscription quota reads live.** With the claude.ai Cookie header in
  the account's `management_key`, Quota reads `claude.ai/api/organizations/
  {org}/usage` (five-hour and weekly windows); the dialog also shows the
  Opus/Sonnet windows and the per-product split of the week, names the source,
  and says why it fell back to the bridge's last reading when the live read
  failed.
- **Antigravity subscription bridge** (`EZLLM_AGY_BRIDGE=on`, off by default).
  A personal Google Antigravity plan as an `openai-compatible` account through
  the official `agy` CLI 1.4.7: text chat with usage, no client tools, the
  answer in one chunk. Setup: `docs/agy-bridge-runbook.md`.
- **One-click bridge accounts.** Add provider offers a preset per bridge that
  shows whether it is running and needs no key; new `GET /admin/bridges`.
- **Dashboard:** Neighbourhood page; lighter login backdrop.

---

## v1.2.0 — 2026-10-24

Feature release; one additive schema step (v28 -> v29, `models.reasoning_vocab`
and `models.reasoning_required`). The columns are additive and NULL means
"nothing known", so an older binary reads a v29 database without harm — but a
v29 binary refuses a v28 database, so roll back from a backup taken before the
upgrade.

- **Reasoning effort follows each model's own vocabulary.** A refusal that names
  the accepted values (`Supported values: [minimal, low, ...]`), or says
  reasoning cannot be disabled, is stored per account and model; later requests
  are clamped down into it instead of being refused. A clamp never raises the
  effort, and an effort the model already accepts goes out byte-identical. Both
  the key test and live traffic teach it.
- **A clamp is never silent.** The console records what the client asked for and
  what the hop received, and a mandatory-reasoning drop is reported as a drop.
- **A 403 that names model access parks that model on that hop** for the normal
  cooldown when another candidate exists, so a block stops re-dialling it and
  losing its prompt cache to a fallback.
- **The per-key model allowlist fails closed** — a store error answers
  `503 authorization_unavailable` rather than suspending the allowlist.
- **Key tests:** a Copilot credential refused at token exchange counts as proven
  discrimination; an endpoint-mismatch refusal is re-probed on `/responses` and
  the wire recorded, so responses-only models stop failing a working key; a
  failed probe writes its reason into the ledger `err`.
- **Console logging:** the access line names the requested target and carries
  the failover trail; auth refusals (missing, invalid, expired, wrong role) and
  the client-budget refusal are visible; a learned vocabulary and a recorded
  wire are reported; the boot states the effective level. `EZLLM_LOG_FORMAT=json`
  for collectors. Contract and measured cost: `docs/logging.md`.
- **Dashboard:** the Connect page reads `GET /admin/routable-models` through the
  session, so its "supported" chips are live observations; the unused
  localStorage bearer path is gone.
- **Leaks:** a parked fallback response and a failed Gemini audio rewrite close
  their upstream bodies on every exit.

## v1.1.1 — 2026-10-23

Fixes only; one additive schema step (v27 → v28, `pending_usage_meter`). An
older binary refuses a v28 database, so roll back from a backup taken before
the upgrade.

- **Security boundaries:**
  - Cookie-authenticated admin writes and sign-out from another origin are
    refused with `403`, even from another port on the same host;
    `SameSite=Strict` alone let such a page mint an admin token with the
    operator's session.
  - Provider and key-test clients no longer follow a redirect to another
    origin, so a 307 cannot carry `x-api-key` and the prompt to another host.
- **Compression sessions:**
  - An explicit `X-Ezllm-Session` is scoped to the authenticated client.
  - A replayed `tool_ref` reference ships only while its target holds the
    same output.
  - A session keeps running the profile snapshot it was bound to, so its
    version stamp names the rules that actually ran.
  - The memo enforces its per-session entry cap, holds at most 32 MiB per
    session and 256 MiB in all, skips forms over 4 MiB, and sweeps idle
    entries every minute.
  - A file read that follows a memoized tool call stays byte-identical.
  - Caveman lite no longer turns "a small number of" into "many", and keeps
    "at the end of the day" and "I think that".
- **Dashboard:**
  - A Policy save no longer moves a key's expiry it did not change.
  - Provider pricing saves again (the page sent `model_id`, but the API
    expects `model`).
  - Chat shows a stream error or a cut-off stream as a failure and keeps the
    partial answer out of later history.
  - Analytics ignore a late response for an older selection.
  - A grouped rule action that fails partway reloads what was stored and
    names the rules that changed and the ones that failed.
- **Metering and pricing:**
  - Only USD rates are accepted; stored non-USD rates count as unpriced.
  - Usage is captured from the upstream wire before translation back to the
    client, including multi-line SSE events.
  - Usage reported by retries and failed hops is ledgered.
  - A meter debit that failed to apply is replayed exactly once before the
    next budget admission (schema v28).
- **Recovery:**
  - Master keys are preserved on read errors and when two boots race.
  - The legacy combo rebuild is atomic and recovers an interrupted copy.
  - `scripts/backup_sqlite.py` takes a verified, WAL-safe backup of the
    database and key.
  - Configuration exports are versioned, include policy and pricing, and
    mark completion explicitly.

- **Request fidelity:** Chat-to-Responses preserves completion limits and
  refuses conflicting limit spellings; Messages-to-Responses preserves
  disabled tool selection. Function definitions retain explicit strict settings.
- **Tool identity:** Messages requests disambiguate tool IDs that would otherwise
  collide after sanitization, keeping each result paired with its call.
- **Refusal streaming:** Chat refusal fragments remain readable until the actual
  finish reason; truncated streams and content after finish still fail.

- **Bounded Codex delivery:** queued output is capped by bytes and event count;
  backlog overflow reports `output_backpressure` and reaps the child even while
  the client sink is blocked.
- **Concurrent transport cleanup:** Claude web fetcher closing synchronizes with
  its first transport initialization.
- **Accurate report caching:** completed failed computations retain their error,
  and explicit usage time filters preserve their full precision in cache keys.
- **Audit fixes:**
  - The Claude web provider verifies TLS certificates and opens a fresh
    upstream conversation per request instead of chaining every caller into
    one.
  - Rules retry the real store after a failed boot load.
  - Compression exempts the final message of a one-message request.
  - The Gemini web bridge resolves its client under the per-account lock.
  - JSON bodies must end after their value; admin bodies over 1 MiB get `400`.
  - Shutdown drains in-flight handlers and background probes before the
    ledger closes.
  - Bridge children end with their request.
  - A budget reservation is released by the ledger flush that counts it.
  - The dashboard CSP admits `data:`, `blob:` and `https:` image and media
    previews.
- **Performance:**
  - Session memo eviction runs in O(1).
  - The shape hash streams instead of marshaling the request history.
  - The local-timezone day report is faster.
  - Usage reports are shared for 10 s (`Age` header), and the dashboard stops
    polling in hidden tabs.
  - Request bodies of known length are read into one buffer.
  - The Claude web provider reuses one HTTP/2 connection.
  - Dashboard pages load on demand (sign-in screen 290 KB → 124 KB gzipped).

---

## v1.1.0 — 2026-10-23

- **ChatGPT subscription through the Codex bridge** (`internal/codexbridge`,
  off by default, `EZLLM_CODEX_BRIDGE=on`): the official `codex app-server`
  behind a loopback OpenAI-compatible surface, locked down so Codex runs no
  commands of its own; client tools through code-mode with continuation,
  every child confined by Landlock; structured output, reasoning summaries
  and an `exec` fallback. See `docs/codex-bridge-runbook.md`.
- **Plan windows on provider cards:** the Claude and Codex bridges serve their
  5h/weekly usage on `/v1/usage`, read for loopback accounts.
- **Key test** advances past models the endpoint does not serve and no longer
  blocks a proven key on a non-auth generation failure.
- **GitHub Copilot** presents CLI 1.0.103 (GitHub's HTTP 466 floor rejected
  1.0.81-6); `GITHUB_COPILOT_CLI_VERSION` passes through compose.
- **Deploy change:** `compose.yml` is no longer shipped. Copy
  `compose.example.yml` to `compose.yml` (gitignored); `compose.traefik.yml` is
  gone. Back up your `compose.yml` before pulling.

---

## v1.0.0 — 2026-10-22

First stable release. Binaries for linux, macOS and Windows (amd64 and arm64
where applicable) on the GitHub release; the image `ghcr.io/c4lyp5o/ezllm` is
now built for `linux/amd64` and `linux/arm64` again, cross-compiled by Go
on the runner instead of emulated, so arm64 costs no QEMU time.

- **Behaviour change: `translation.enabled` is the one translation switch.**
  `translation.universal_enabled` and `EZLLM_TRANSLATION_UNIVERSAL_ENABLED`
  still work with a boot warning; an explicit `universal_enabled: false` now
  turns all translation off.
- **Every environment variable documented** in `docs/environment.md`, with
  what a new install sets first.
- **Fix: a retired model (410 Gone) no longer fails a key test**; the probe
  moves to the next candidate.
- **Fix: Ollama (local) keeps the operator's base URL**; the models drawer
  refreshes after a catalog sync; quota percentages render whole.
- **CI: a release tag must match `internal/server/version.go`.**

## v0.0.21 — 2026-10-21

- **Claude subscription M2: tools.** claude-sub now accepts client tools and
  returns `tool_use` under the client's own names, streaming and non-stream; the
  M1 `chat_tools_unsupported` refusal for all tools is gone (server tools and
  `parallel_tool_calls` are still refused). Tools ride in the extra body; native
  runs none of them. Hermes agents work through the normal OpenAI → Messages
  translation. The bridge keeps only the newest two cache markers, since native
  spends two of upstream's four (`Found 6` was a live 400).
- **claude-bridge CLI pinned at 2.1.306 as a copy.** The auto-updater had deleted
  2.1.299 from under the live mount; the pin now lives in
  `~/.local/share/claude-bridge/`.
- **Fix: the first catalog refresh waits 30 s after boot.** It ran before the
  Claude bridge and the listener were up, so claude-sub failed (and raised a
  bell entry) on every restart.
- **No default dashboard password (schema v26).** First boot prints an ezllm
  banner with a one-time setup code; the dashboard shows only a set-password
  page until the code and a new password are entered (`first_boot` flips).
  `EZLLM_DASHBOARD_PASSWORD` / `_FILE` skips setup. Databases still on the
  old published `STRONGPASSWORD` return to setup on upgrade and lose their
  sessions. Login now rate-limits empty passwords too, and the 401 no longer
  carries `default_password`.
- **Fix: the VS Code path token now hits the per-key budget.** It shared
  bearer auth's lookup but skipped the budget check, so a capped key kept
  working through `/api/v1/vscode/{token}/…`. Both now run one gate.
- **Fix: the access log no longer prints VS Code path tokens.** The alias
  carries a client token as a URL segment; the log line now shows
  `/api/v1/vscode/***/…`. A reverse proxy in front must redact it too.
- **Fix: behind a reverse proxy, one guesser no longer locks everyone out.**
  The sign-in/setup limiter keyed on the socket peer, which behind traefik is
  traefik for every client. `EZLLM_TRUSTED_PROXIES` names the proxies whose
  `X-Forwarded-For` it believes; the traefik overlay sets it.
- **Fix: sign-in can no longer be used to burn CPU.** Login, setup and
  password change share four concurrent bcrypt slots; past that, `429` with
  `Retry-After`.
- **Fix: the Claude bridge no longer leaks on failed requests.** An early
  return (timeout, client disconnect) left a blocked reader goroutine and an
  unreaped zombie CLI process each time.
- **Cleanup**: staticcheck now reports nothing (dead code removed, the one
  intentional deprecation documented); one table maps resolution errors for
  both the wire and the ledger; the web bridges share one admission check;
  multipart field parsing and body peeking each live in one place; the
  model-protocol upsert is written once. Fixes found on the way: a peeked
  upstream body no longer swallows `Close` (a connection leak on abandoned
  responses), a sweep batch no longer writes service-surface verdicts into
  the chat column, abandoned error bodies drain at most 64 KiB, the field
  cipher is built once, probe bodies quote model ids as real JSON, expired
  dashboard sessions are pruned at login, a renamed combo's held hop can be
  released again, and a CORS test that could never fail now asserts.
- **Notification bell** (top right; schema v27): new, gone and broken free
  models, failed catalog and price refreshes, and client tokens at their
  budget. Repeats fold into one entry with a count; clicking one opens the
  page it concerns. `GET /admin/notifications`, `POST …/read`.
- **Catalog saves are observable**: every save logs exactly which models came
  and went, the Providers page shows when each catalog last synced, and models
  that just arrived are priced at once instead of at the next daily run.
- **Fix: "Test all" no longer deletes proven models from the catalog.** Its
  untested-only filter ran in place over the very list the catalog was saved
  from, so every model that already had verdicts was dropped (with them).
  And a test-all with nothing left to probe now saves the catalog it listed
  instead of answering "synced" and keeping the old one.
- **Fix: audio transcription and image edits answer 429 when rate-limited.**
  The multipart path kept its own copy of the post-dispatch code and had
  drifted: it answered 502, and skipped hop health and the failover trail.
  Both paths now share one `finishDispatch`.
- **`least_used` works.** It was accepted and documented but kept hop order.
  Each combo hop now carries a recent-traffic count (halving every 10
  minutes) and the least-loaded hop goes first; it is back in the combo
  form's strategy picker.
- **Fix: "Log out" now ends the session.** It cleared only browser storage,
  so the HttpOnly cookie kept opening `/admin` for 12 hours.
  `POST /admin/logout` revokes it server-side.
- **Fix: request bodies have a read budget** (30 s for admin JSON, 2 min for
  inference) and idle connections close after 2 minutes. A client could
  trickle a body forever and pin a handler — on `/admin/login`, without
  authenticating. Streaming responses are unaffected.
- **The dashboard shell sends a Content-Security-Policy** (scripts from this
  origin plus the theme bootstrap by hash, no framing) and every response
  sends `X-Content-Type-Options: nosniff`.
- **Toolchain pinned to go1.27.2**: picks up ten standard-library security
  fixes (HTTP/2, MIME header limits, Range parsing); govulncheck reports
  nothing reachable.
- **Password strength bar** on the first-boot setup page and in Settings:
  four segments plus a hint, estimated locally (length × character pool, cut
  for repeats, keyboard runs and well-known passwords). It guides; the server
  still enforces only the 12-character minimum.
- **The first-boot setup code stands out**: a framed panel under the ezllm
  art, with the code spaced out and, on a terminal, the only filled
  highlight in ezllm's output.
- **Catalogs refresh every 12 hours** (was 6), and **new free models are
  probed as soon as they land**: after a refresh, a new key or a manual
  sync, the account's free models are protocol-probed on every surface
  still untested (they cost no credits), and the auto-free combos are
  rebuilt from the verdicts, so a free model broken everywhere drops out
  immediately instead of at the next refresh. The refresh moved into the
  server next to the price refresh.
- **Console logs revamped**: timestamp, padded level and `key=value` on one
  line, with the level colored on a terminal (`EZLLM_LOG_COLOR`,
  `NO_COLOR`). `EZLLM_LOG_LEVEL` accepts `info` and warns on an unknown
  value; package-level `slog` calls now share the same format.
- **Cloudflare free models join auto-free combos.** Workers AI's free tier has
  no marker in the model id, so a built-in list (65 models, task class kept
  as the dashboard states it) names them. Auto-free combos take the 31
  chat-capable ones, on a host-verified Workers AI account only; embedding,
  image, audio, translation and classifier models stay on the list as data
  but never become chat hops.
- **Five more service endpoints**: `/v1/images/edits` (multipart),
  `/v1/moderations`, `/v1/rerank`, `/v1/videos/generations` and
  `/v1/music/generations`, each with a Services card, a test probe and a
  pinnable surface.
- **Fix: service endpoints no longer read the chat probe.** A model whose chat
  probe failed (every embedding, image or rerank model) was refused on its own
  endpoint; service surfaces now read as untested there.
- **Fix: pins on service surfaces.** A model pinned to `embeddings` (or any
  service surface) was refused on its own endpoint, a chat pin admitted
  rerank/moderation requests, and the multipart endpoints answered pin and
  wire refusals with 502 instead of 400. A pin now admits exactly its own
  endpoint everywhere, with the same 400 `pinned_surface` refusal.
- **Services "Try it".** The media services (image generation and editing,
  text to speech, speech to text, video, music) can be generated from their
  card with the saved default model; images, audio and video play inline.
- **Local Ollama preset** (`http://localhost:11434/v1`, any placeholder key).

---

## v0.0.20 — 2026-10-20

The compression stack grows to ten engines, wire translation becomes the
default, and the dashboard gets a data-terminal redesign.

- **Five new compression engines**, all on one wire-neutral text walker so a
  request is transformed identically whether it arrives on chat, messages or
  responses: `ansi` and `fold` (terminal output), `jsonmin` (whitespace-free
  JSON tool output), `tool_ref` (repeated tool results become references) and
  `blobs` (base64 runs and data URIs become markers). The `budget` engine is
  **deleted** — it needed a known `max_tokens` and silently did nothing
  without one.
- **The pipeline is now ordered by meaning.** A per-profile `tool_classes`
  table says what each tool is (file read, edit, diff, …); a file read now
  ships byte-identical so the agent's next edit still matches, and an unknown
  tool is its own class — never truncated, only tidied by the lossless
  engines. `fresh_tool_output` decides whether a final message that is purely
  tool output may be trimmed on its first send (schema v24).
- **A committed-prefix memo** per request, so the cached prefix is exactly
  what was committed; when dedup drops a segment the commit is exact rather
  than approximate.
- **Universal wire translation is default-on** (no yaml needed; an
  anthropic-only account is pre-marked so the universal matrix translates
  instead of refusing). One translation path in `commit`, a `gatewayFault`
  helper, and the `zz_` files renamed.
- **A hop that refuses the WIRE now fails over.** An upstream 400 that names a
  wire-protocol gap (`ModelProtocolUnsupported`, `not supported for format`)
  describes the HOP, not the body: the next hop serves the identical request
  and the refusing hop is parked. Ordinary 400s still pass through unchanged,
  and a sole hop still returns its own 400 verbatim. Motivated by
  `daily-pro`: the opencode-go hop refused `/v1/messages` while the
  command-code hop was never tried.
- **Combo hop auto-probe.** A combo write (`POST /admin/combos`, `PATCH`,
  `POST|PUT …/hops`) starts a background protocol probe of that combo's
  ENABLED hop models only — never the account catalog — on the surfaces still
  lacking a verdict. An account sweep samples a 500+ model catalog, so the hop
  an operator actually routes through was often never sampled. Curated rows,
  disabled hops and disabled combos cost nothing.
  `EZLLM_COMBO_AUTOPROBE=off` disables it.
- **The proxy bills every input token into one bucket**, honours the account
  usage convention, and splices the hop model into the client bytes instead
  of re-marshalling them. A hop stays parked after a transport error, and an
  oversized body is a 413.
- **Provider and registration honesty**: quota and catalog reads honour
  custom headers, pagination and no-endpoint statuses; the management key
  stays on tokenrouter; retries honour http statuses and sweeps keep proven
  verdicts; a claude-web key keeps its wire.
- **Perf**: one catalog read per hop, an in-memory routing catalog snapshot
  invalidated by `catalog_gen` (schema v23), a throttled reservation prune
  and an allocation-free header strip.
- **The dashboard is a data terminal** — exact numbers (never abbreviated),
  motion.dev throughout, and a screen-by-screen polish pass verified in a
  browser. The build is now reproducible: `web/src/index.css` excludes
  `web/dist/` from Tailwind's content scan, because `dist/` is a tracked
  build output and Tailwind was reading the previous build's class names
  back into the next one.
- **The roster and rules re-read.** Providers is a roster: one expandable row
  per account with a fixed-width quota column, so every figure sits on one
  right edge instead of ragged per-card alignment. Rules are one table row
  per SETTING rather than per model — rules on the same provider with the
  same cap, window, state and note collapse into one group listing its
  models (the wire stays one rule per account+model; a group is a view, not a
  schema change). The Dashboard's health strip becomes a header pill: an
  uptime indicator, with the account/model/combo counts dropped from it
  because inventory is not health and those counts live on their own screens.
  Labels are sentence-case throughout.
- `unmetered_rows` on health, overview, live feed and dashboard; the tz day
  report's `applied_saved`; the meter is all-or-nothing per batch; web
  bridges now require a process secret.
- **CI actions move off deprecated Node 20.** Eight pins were still on Node
  20 targets that the runners have deprecated and were force-shimming onto
  Node 24: `actions/checkout@v4`, `actions/setup-go@v5`,
  `actions/setup-node@v4`, `docker/setup-qemu-action@v3`,
  `docker/setup-buildx-action@v3`, `docker/login-action@v3`,
  `docker/build-push-action@v5` and `softprops/action-gh-release@v2`. They
  are now `checkout@v5`, `setup-go@v6`, `setup-node@v6`,
  `setup-qemu-action@v4`, `setup-buildx-action@v4`, `login-action@v4`,
  `build-push-action@v7` and `action-gh-release@v3` — every pin resolves to
  a Node 24 action natively, and a pin audit asserts it.
  `setup-go@v7` and `setup-node@v7` were skipped on purpose: both are ESM
  migrations with breaking changes, and v6 already carries the
  `@actions/cache` and `undici`/`fast-xml-parser` security updates.
  `build-push-action` had no such choice — v5 and v6 are both still Node 20,
  so v7 is the only Node 24 line, and its breaking changes (the ESM switch
  and dropping two deprecated env vars this repo never set) do not touch the
  inputs used here. No product behaviour changes; the gate and the image
  build are otherwise identical.
- **The image build stops pretending to be multi-arch.** The container was
  built for `linux/amd64,linux/arm64`, but the arm64 half ran the whole
  `golang:1.27-alpine` + distroless + Go toolchain under QEMU emulation on
  an amd64 runner — 7 of the 8 minutes of every push — and no host in the
  fleet is arm64 (calypsocloud-1, Media-Server, GAME-PC and the OVH VPS are
  all x86_64). The image is now `linux/amd64` only, the QEMU setup step is
  gone, and the build uses the GitHub Actions layer cache
  (`cache-from`/`cache-to: type=gha,mode=max`) so the Go module download and
  the compile are cached between pushes instead of redone from scratch every
  time. `ignore-error=true` on the export: a cache write can fail after the
  image is already pushed, and the worst acceptable outcome is a cold cache,
  not a red build. arm64 *binaries* are unchanged — `release.yml` still
  ships linux/arm64 and darwin/arm64, cross-compiled natively by Go.
- **Command Code quota**: an optional session cookie (stored in the encrypted
  `management_key`) lets the quota read the account's 5h, weekly and monthly
  limits from the usage page's billing endpoint. Pasting the whole Cookie
  header also shows "expires in N days" in the edit form. OpenAI gets the same
  opt-in: a platform session token reads the credit balance, and so does an Anthropic
  Console cookie (prepaid credits).

---

## v0.0.19 — 2026-10-19

Claude subscription surface: **one container**, no sidecar.

- **Design pivot.** The `claude-bridge` sidecar (M1's first shape) is deleted:
  a second image, a second container, an inter-service token and a published
  port for what is fundamentally "spawn this binary". The surface now runs
  **in-process**, on a loopback-only listener (`127.0.0.1:8790`), and an ordinary
  `anthropic-compatible` account dials it — no new provider kind, no router
  change, no inter-service credential.
- **The CLI is bind-mounted, not baked.** `~/.local/share/claude/versions/2.1.299`
  mounts read-only at `/opt/claude/claude`; the subscription login mounts
  read-write at `/claude-sub` (the CLI refreshes its own token there). The
  account's shared secret auto-generates into `/data` on first boot, like the
  master key.
- **Runtime base: `distroless/base-debian12`** (was `/static`). Measured: the
  Claude Code CLI is a glibc-dynamic ELF (`ld-linux`, libc/librt/libpthread/
  libdl/libm), and on `distroless/static` the exec fails with "no such file or
  directory" because the loader is absent — that failure was observed live.
  Alpine is smaller (7.81 MB) but musl, so it cannot run this binary at all.
  `base` is 20.8 MB and the smallest maintained base that runs it: glibc + CA
  certs + NSS (DNS needs `libnss_dns`, invisible to `ldd`), still no shell. The
  ezllm binary itself remains static.
- Same rails as M1: text-only (`chat_tools_unsupported` before any spawn),
  `CLAUDE_CODE_EXTRA_BODY` body injection, `shouldQuery:false` history replay,
  native error codes mapped to honest statuses, boot asserts the CLI version and
  flag surface (`--max-turns`/`--system-prompt-file` are accepted but absent from
  `--help` on 2.1.299).
- **Probe budget (the NIM lesson, again).** The key-test generation probe and the
  protocol-sweep surface probe budgeted **8–16 output tokens**. Measured: a model
  with mandatory thinking (Claude Haiku 5.5) cannot answer in that and returns
  `max_output_tokens`, so a perfectly good key failed the inference step — and the
  sweep would misread a Messages-only upstream. Probes now budget 256 tokens
  (`internal/registration/probe_body.go`), with a test that fails if anyone
  shrinks it.
- `EZLLM_CLAUDE_BRIDGE=on` enables it; without a usable CLI or login the boot
  fails loudly. Loopback is enforced — the surface refuses a non-loopback bind.

## v0.0.18 — 2026-10-19

Claude subscription sidecar (`claude-bridge`) — milestone M1, text-only.

- **New `cmd/claude-bridge` + `internal/claudebridge`**: an Anthropic Messages
  surface that drives the official Claude Code CLI against a Claude subscription
  login. The CLI is the only thing that speaks to the subscription — the
  credential never leaves its own login store. ezllm dials the bridge as a plain
  `anthropic-compatible` account, so no router kind, surface or translation cell
  changes.
- **Upstream body injection**: the client's Messages body reaches upstream through
  `CLAUDE_CODE_EXTRA_BODY` (the official plugin's mechanism), with `system` held by
  `--system-prompt-file` and stdin frames marked `shouldQuery:false` on replay.
  Measured on CLI 2.1.299: the body drives the answer (a body saying "seven" beats
  trigger frames saying "eight"), one generation per call, and the CLI's own
  ~20k-token harness overhead disappears (170 input tokens for a three-message
  body, vs ~20k on the naive `-p` path).
- **M1 is text-only and says so**: tool-bearing requests are refused with
  `chat_tools_unsupported` before a process is spawned.
- Error mapping keeps the CLI's words and names the cause: `rate_limit`→429,
  `billing_error`→402, `authentication_failed`→401, `overloaded`→529,
  `max_output_tokens`→400 (a client-fixable ceiling, not a retryable 5xx).
- Live proof `tests/prove_claude_bridge.py`: 13/13 against the real CLI and a real
  Pro subscription — streaming SSE, native usage fields, refusal, and history
  fidelity.
- `Dockerfile.claude-bridge` + `compose.claude-bridge.yml`: distroless-static
  image; the CLI binary (265 MB, read-only) and the login directory are mounted
  and pinned by mount path; no published ports.

## v0.0.17 — 2026-10-19

Same-day batch: image content across all three wire surfaces (`f13ffdc`);
geminiweb re-scrapes the SNlM0e access token after PSIDTS rotation and reverts on
failure (`ce5c2dc`).

---

## v0.0.16 — 2026-10-19

Claude (web cookies) multi-turn conversations.

- **Follow-ups chain the server's assistant uuid**: the assistant message
  uuid is assigned by Claude in the `message_start` SSE event, not by the
  client. Chaining the locally generated placeholder returned an empty-body
  400 on turn 2. `consumeSSE` now captures `message.uuid` and commits it as
  the next turn's `parent_message_uuid`.
- **Follow-ups carry a fresh `human_message_uuid`**: the payload previously
  sent only `conversation_id` + parent on continuation turns; the live wire
  requires the human uuid too (also an empty-body 400).
- Non-2xx errors now include a bounded upstream body snippet, so a bare
  "status 400" is diagnosable.
- Tests: `TestFollowUpPayloadCarriesAllThreeUUIDs`,
  `TestConsumeSSECapturesServerAssistUUID` (both bite).

## v0.0.15 — 2026-10-19

Claude (web cookies) completions work against the live wire.

- **Full cookie header is required**: Claude/Cloudflare answers a
  sessionKey-only request with a synthetic `429 rate_limit_error` in ~30ms;
  the same completion carrying the browser's `cf_clearance` / `__cf_bm` /
  `anthropic-device-id` / `sessionKeyV3` cookies succeeds (measured 30ms 429
  vs 1.25s 200). `NormalizeSessionCookie` now preserves the operator's whole
  header (still accepting a bare token) and the bridge sends
  `anthropic-device-id` from it.
- **Streaming timeout fix**: `TLSFetcher.Fetch` cancelled its request context
  on return, so every SSE read died with `context canceled` and 429 bodies
  were misreported as transport errors. The cancel is now tied to
  `Body.Close()`; `TestCancelOnCloseKeepsBodyAlive` guards it.

## v0.0.14 — 2026-10-19

Gemini (web cookies) generation is live.

- **geminiweb generate**: the request now matches the reference client's wire
  identity — `_reqid` seeds a random 5-digit value (bumped by 100000 per turn),
  the turn uuid is shared between payload slot 59 and the
  `x-goog-ext-525005358-jspb` header, and the model header always carries a
  per-client session uuid. `TestGenerateWireIdentity` guards all three.
- **geminiweb stream framing**: strip the `)]}'` anti-XSSI prefix before the
  length-prefixed frames, unwrap the one-level `[["wrb.fr",…]]` envelope, and
  count the declared frame length the way Google does (it includes the
  separator newline). `TestGenerateRealWireShape` resumes the live shape.
- **Local test ergonomics**: `make test-quiet` pins the suite to cores 0–1 with
  `nice`/`ionice`, and `make check` refuses to run unbounded locally unless
  `EZLLM_ALLOW_FULL_CHECK=1`. CI remains the full gate.

## v0.0.13 — 2026-10-18

Native image/audio surfaces and the playground dashboard pass.

- **Native image adapters**: Cloudflare Workers AI image models now use
  `/ai/run/<model>` (flux-2 multipart, other models JSON `prompt`/`seed`)
  instead of the OpenAI `/images/generations` path.
- **Native audio adapters**: Cloudflare TTS/STT use `/ai/run/<model>` with
  MeloTTS `{"prompt": ...}` translation; Google AI Studio TTS/STT translate
  OpenAI audio bodies to native `generateContent` and back (PCM wrapped as WAV,
  STT text in OpenAI shape). Tests in `internal/proxy/gemini_audio_test.go`.
- **Dashboard**: Linear-inspired palette and sidebar icons; Providers models
  render as a compact 4-column status grid with per-row test indicators;
  Services use a two-column card grid with surface badges.
- **Chat is a playground**: 1–3 side-by-side columns, each with its own model
  selector and transcript, one detached composer broadcasting to every
  selected model. Added columns carry the remove control; the first does not.
- **Dashboard**: new routing-rescue panel surfaces recent calls that failed
  over and the hop trail that saved them.
- **Security (Milestone A)**: provider API keys are encrypted at rest with
  AES-256-GCM. The master key comes from `EZLLM_MASTER_KEY`, else
  `~/.hermes/secrets/ezllm-master.key` (auto-generated 0600 on first boot).
  Existing plaintext rows are rewritten in place on boot, idempotently;
  `PickKey` decrypts only for the outbound hop. Tests in
  `internal/store/crypto_integration_test.go`. **Back up the master key with
  the DB — losing it loses every provider key.**
- **Provider quota**: added TokenRouter management-key balance oracle
  (`/api/management/self/wallet`), parsed into a money quota. The management key
  is an optional per-account credential (`management_key`, schema v19), stored
  encrypted at rest and entered in the Add/Edit provider form; the API exposes
  only `management_key_set`. Tests in `internal/store/crypto_integration_test.go`
  and `internal/provider/quota_tokenrouter_test.go`.
- **Security (Milestone B)**: `/admin/login` now also sets an HttpOnly,
  SameSite=Strict `ezllm_session` cookie (Secure on HTTPS). The admin
  middleware accepts that cookie, so the dashboard no longer stores the admin
  token in `localStorage`; it sends `credentials: include`. Five failed
  password attempts per IP in fifteen minutes answer 429 (empty-password
  probes are exempt). Tests in `internal/server/dashboard_auth_test.go`.
- **Price catalog API**: `GET /v1/prices?model=&account_id=&at=` returns the
  current/historical rate per account+model; `POST /admin/prices/import` bulk-upserts
  snapshots; `POST /admin/prices/refresh?source=litellm` pulls LiteLLM's public
  price map and matches by exact model-id suffix. Prices carry `source` and
  `fetched_at`; schema v22. The refresh runs **in-process** (~45s after boot,
  then daily at 03:00 in `EZLLM_USAGE_TZ`), so a self-hosted instance needs no
  external cron.
- **Fix**: key retest and provider catalog refresh decrypt the stored key before use;
  encrypted keys no longer reach providers as `enc:v1:`.
- **Per-key budgets enforced**: client token `cap_tokens`/`cap_window` meter in
  `client_usage_counters` (schema v20) inside the ledger transaction; a spent
  key returns `429` before dispatch. Caps set after usage backfill the current
  bucket from `calls` once. `max_tokens` is reserved before dispatch and released
  on return; reservations expire after 15 minutes. Schema v21 adds
  `allowed_models` (glob allowlist, `403 model_not_allowed`) and `expires_at`
  (`401 token expired`). Tests in `internal/store/client_budget_test.go`,
  `internal/store/client_policy_test.go`, `internal/server/client_budget_test.go`,
  and `internal/server/client_policy_test.go`.
- **Web providers (claude-web/gemini-web)**: the bridge now serializes
  generation per account (one in-flight turn per cached client) and moves
  Gemini cookie rotation under the same lock; the Claude client decompresses
  zstd/br/gzip responses and accepts the live API's numeric organization id.
  Live-verified: org discovery resolves on a real session.
- **Streaming**: a mid-stream upstream connection drop now ends the stream
  with a terminal SSE error frame in the client's dialect (OpenAI, Anthropic
  or Responses) instead of a silent EOF; the ledger still records the
  truncation.
- **Dashboard chat**: `chatStream` works with the httpOnly cookie session
  (no admin token in localStorage required) and sends `credentials: include`,
  fixing the red "unlocked" box with zero network activity.

---

## v0.0.12 — 2026-10-17

Repo housekeeping and version plumbing.

- **Versioning**: new `VERSION.md` (this file); `internal/server/version.go`
  single source of truth; `GET /healthz` now reports `{"status":"ok","version":…}`;
  `/admin/overview` health object carries `version`; dashboard health strip shows it.
- **Docs**: README rewritten (professional, no personal deploy details);
  CONTRIBUTING.md and AGENTS.md added; EXPLORING.md/API.md/CHANGELOG synced
  to shipped state; stale landmarks corrected.
- **Tests relocated**: live proof scripts `scripts/prove_*.py` → `tests/`;
  Playwright drivers `web/shot-*.mjs`, `web/verify-*.mjs` → `web/tests/`.
  Go `_test.go` files stay in-package (Go toolchain requirement — see CONTRIBUTING).
- **Makefile**: test targets carry CI's memory envelope (`GOMEMLIMIT=3GiB`,
  `GOGC=50`), `test` runs `-count=1 -timeout 15m` (CI parity; cached results
  were masking changes), new `test-short` for constrained environments.
- **CI**: `release.yml` tag builds gated by the reusable `test.yml`
  (`needs: test`) — a red suite never produces a release asset either.
- **Provider credential entry (both web protocols)**: GitHub device-flow stall
  fixed — `slow_down` is now surfaced (`GHDeviceStatus.slow_down`) and the
  dashboard raises its poll interval (RFC 8628 §3.5) instead of folding it into
  plain pending forever; poll/mint run on a detached context so a tab reload
  cannot cancel an already-granted authorization; 5xx/429/context errors are
  retryable instead of terminal (one blip used to poison a live handle); the
  pending panel now renders `detail`, every status failure reaches `ErrBox`
  (non-ApiError was silently swallowed), and the flow logs (handle prefix only).
- **gemini-web cookies**: the key field is a textarea (`multilineKey` preset
  flag) and `ParseKey` accepts a WHOLE cookie header (`Cookie: a=b; c=d`,
  `document.cookie`, or one per line), keeping only `__Secure-1PSID`/TS by
  EXACT name — the old line-wise split stored the entire rest of the header as
  the PSID value. Guards: 4 new ParseKey tests + `slow_down`/classification
  tests, all three bite-proven red without their fix.
- **Combos hop account select**: options grouped by provider (`<optgroup>` from
  `base_url`, count shown only when a provider has >1 account) so multi-account
  providers are visible without a schema change.

## v0.0.11 — 2026-10-16 · 30 commits (02b1c40..75a46e5)

The four web-protocol providers, compression addenda, and the CI gate.

- **M11 gap closure**: 1 MiB percentiles, fuzz moat, cancel-latency certificate.
- **M12 gemini-web**: wrb.fr frame parser, cookie auth, GET_USER_STATUS oracle,
  StreamGenerate + incremental frames, provider kind + in-process bridge; runbook.
- **M13 github-copilot**: device flow, mint chain, routing, quirks, catalog,
  adapter + `ezllm github-login`; Copilot third-party OAccess policy evidence.
- **M14 claude-web**: uTLS Chrome transport, session auth, SSE conversation
  state machine, provider adapter + loopback bridge; runbook.
- **Compression addenda** #2–#7: hop compatibility groups + context-size
  filtering, request-wide attempt budget + selection deadline, immutable
  profile versions, absolute floor + CPU cap + persisted decision versions,
  per-engine attribution + sampled shadow mode, rtk tool-class filters.
- **Correctness**: caveman §11 rule safety, one authoritative applied-savings
  aggregate (WS-A), disjoint input/output economics (WS-B), committed-prefix
  stability (WS-C).
- **CI**: `test.yml` gate (gofmt + vet + test + web build) now blocks
  `docker-publish` via `needs: test`.
- **Web**: provider presets for the three web-protocol providers, Copilot
  device-flow login, bar-chart ordering, railway backdrop restraint, token
  economics cold-input fix.

## v0.0.10 — 2026-10-15 · 11 commits (485ffc7..e5033eb)

Performance baseline and the single-parse dispatch.

- **M1 perf baseline**: phase benchmarks, latency percentile harness,
  cancellation / slow-consumer / retained-memory tests (no leak: ~1.3–2 KB/req).
- **M10 single-parse dispatch**: one JSON decode per request through
  extractModel → Apply → captureShape → prepareHopBody (−25% e2e non-stream,
  −26% stream at 1 MiB; design `docs/single-parse-dispatch-design.md`).
- **Providers**: capacity-shaped generation failure passes with a proven key,
  sweep registry can no longer wedge, real NDJSON step chain, transient
  probe-status badge, drawer retest-all + live status.
- **Web**: login page (first-run hint, railway backdrop), dashboard polish
  (analytics, economics, rules, requests, compression), repo file sort.

## v0.0.9 — 2026-10-14 · 10 commits (abd130b..44b9901)

Service-level endpoint defaults and host-scoped quota.

- **Schema v15**: `service_bindings` + `service_probes` for per-service
  endpoint defaults (embeddings / images / audio TTS / audio STT).
- **Server**: per-service default bindings with honest typed refusals;
  multipart STT gets a missing model field injected.
- **Quota**: host-scoped same-key quota oracles, `ezllm` UA on own requests,
  `tokens_today` / `requests_today` wired end-to-end, count-shaped quota
  rendered on provider cards, token kind shown in key tests.
- **Web**: Services page (endpoint defaults, honest states, prompt tests),
  cache split carried onto every client wire, docx UI list.

## v0.0.8 — 2026-10-13 · 18 commits (f7ee36b..4232902)

Universal wire matrix, four new inference endpoints, and key-test hardening.

- **Universal wire matrix**: surface-neutral request IR, shared response/event
  codecs, per-account Messages usage convention (schema v13), translated hops
  as strategy citizens, single `translation.universal_enabled` switch.
- **Endpoints**: embeddings, image generation, audio TTS + STT (multipart),
  with per-surface default models (`/admin/services` contract).
- **Key tests**: NVIDIA NIM measured-fast probe models + timeout advances to
  the next candidate; Cloudflare Workers AI account-aware oracle with absolute
  probe URL and `@cf/…`-name catalog ids; plan-free model probe; retired-model
  and empty-balance survival.
- **Web**: searchable chat model picker, per-model Test column, 7-surface
  model-drawer pinning, NVIDIA NIM / Cloudflare / OpenRouter presets,
  caller IP + in/out token fold on recent calls.
- **Aliases**: VS Code tokenized aliases (OmniRoute parity).

## v0.0.7 — 2026-10-12 · 37 commits (8afa172..3a3ab3c)

The translation day: anthropic↔openai and chat→responses, probe-gated.

- **anthropic→openai translation**: wire-protocol fixtures, deterministic
  request translation (golden-tested), openai→anthropic response translation,
  SSE synthesis, config knob + hop eligibility; translated hops ordered after
  native.
- **responses-wire translation**: chat client → `/v1/responses` hop,
  curated responses-wire hops for chat clients; anthropic↔responses leg
  (Claude Code on responses-only hops).
- **Visibility**: hop-aware routing with visible failover, block-position
  tagging + failover trail, block-cursor advances logged, upstream error
  cause classification + cause chips, consolidated recent-calls table,
  combo hop-test modal, request IP.
- **Verdicts**: curated protocol verdicts (schema v11), whole-catalog protocol
  sweep ("test all"), wire identity + wire-first selection.
- **Web**: Vercel AI Gateway preset with per-provider quota, dashboard
  polish pass (10-item list), cause chips + current-state tooltip.

## v0.0.6 — 2026-10-11 · 12 commits (c5053c8..0044b00)

Routing honesty and dashboard rev 2.

- **Router**: true `true_round_robin` (stateless dice roll), success-driven
  `strict_round_robin`, `block_round_robin` block size, the dead success hook
  (`MarkHopGood`) wired for the first time; fail closed on store errors;
  never cache decrypted keys.
- **Registration**: per-test adapter resolution (not per process), plan-free
  probe so a plan refusal is not read as a bad key.
- **Proxy**: a status the upstream never sent is never written; hidden causes
  surfaced.
- **Store/ledger**: lost ledger batch made visible, p50 computed in SQL,
  409 on conflict; repeated user turn never dropped from history; timezone
  that will not load is refused instead of silently falling back to UTC.
- **Web**: dashboard UI rev 2 (analytics explorer, light/dark theme, chat
  upload), day alignment with the meter, disaster-recovery export stream,
  `least_used` dropped from the strategy picker.
- **Presets**: Ollama Cloud.

## v0.0.5 — 2026-10-10 · 21 commits (c099dd5..9d05fa6)

Surface pins, research schema, and honest economics (M8/M9).

- **Surface pin**: per-model `proto_pin` (schema v7) — enforced, never
  rerouted; wrong-surface calls refuse `400 pinned_surface` before any dial.
- **M9 research schema (v8)**: request-shape capture (`prefix_sha`,
  `session_id`, `req_bytes`), versioned model prices + derived cost, real vs
  notional savings split, TTFT measured from dispatch.
- **Admin API**: request-shape + pricing/cost endpoints, `GET /admin/requests`
  token filter, `POST /admin/chat`.
- **Routing**: per-account request budget, block round robin, honest strategy
  implementations; 429 is a failover signal, not a retry signal.
- **Registration**: 400 from the auth oracle defers instead of failing; no
  doubled HTTP prefix in oracle detail.
- **Web**: confirmation modals for destructive actions, cost panel +
  per-account pricing drawer, connect page rewrite, OpenAI preset,
  docs refresh to shipped state.

## v0.0.4 — 2026-10-09 · 15 commits (8c832ef..fb2a2fd)

First hardening pass and preset expansion.

- **Fixes**: client `Origin` stripped before forwarding (Anthropic CORS 401),
  dashboard session cleared only on own auth 401, routed account id persisted,
  duplicate token names rejected, combo config size advertised.
- **Router**: dead combo endpoints cool down; rules refusal returns 429, not
  502.
- **healthz reduced to liveness only** (`{"status":"ok"}`) — detail moved
  behind `/admin/*`.
- **Provider**: `gemini-openai` kind with scoped 400 auth rejection; key shape
  checks dropped — the probe chain is the sole validator.
- **Presets**: Command Code, DeepSeek, TokenRouter, Gemini, Groq.
- **Web**: 18-item dashboard UI pass.
- **Docs**: housekeeping to shipped state (M1–M7), API.md extended, CHANGELOG added.

## v0.0.3 — 2026-10-08 · 21 commits (0056710..e60b7fd)

Containerization, chat, and retry semantics.

- **Retry**: combo hop retried with exponential backoff before failover;
  global policy (3 retries/hop, transient-only) with config knobs.
- **Config**: credentials become SQLite-only; `EZLLM_*` env overrides restored
  for containers (deployment bug fix).
- **Budget**: compression budget stage with own context-token accounting
  (schema v5).
- **Web**: chat page behind the real routing path, Token Economics +
  budget-engine controls, add-provider dialog with key validation on save.
- **Deploy**: static distroless Docker image + home-box Traefik overlay;
  explicit router priorities + dedicated `/admin/login` limiter; MIT license.
- **Combos**: advertised in `/v1/models`, `enabled` honored in PATCH,
  per-combo enable toggle.
- **Presets**: Claude Platform; anthropic-compatible `/v1` join fix.

## v0.0.2 — 2026-10-07 · 18 commits (c10d422..18abe07)

Rules, compression, and the dashboard surfaces (M5–M7).

- **Rules**: eligibility engine — token caps + allowed-use windows, dashboard
  editor with live open/closed chips.
- **Compression**: M5 pipeline (engines, profiles, CRUD, inference wiring),
  headroom + lite engines, caveman (lossy prose condensation gated by
  construction), `rules_fired` attribution end to end, surface stamp on the feed.
- **Ledger**: `GET /admin/requests` filters by tokens in/out; Requests page.
- **Surfaces**: stats explorer (granularity × range), live recent-requests
  feed (SSE over fetch).
- **Auth**: dashboard password + plaintext keys stored in SQLite.
- **Fix**: combo refused by rules on every hop returns 429, not 502.
- **Test**: dashboard auth + plaintext schema coverage; Playwright render
  drivers.

## v0.0.1 — 2026-10-06 · 10 commits (d0247f8..fcabd35)

Initial release: the skeleton up to a working routed gateway.

- **M1**: skeleton + `/healthz`.
- **M2**: SQLite store, field crypto (AES-GCM, test-covered, unwired in v1),
  three-surface passthrough (chat / messages / responses).
- **Store**: admin persistence — accounts, provider keys, combos, client tokens.
- **Provider**: live model-catalog probe + adapter protocol surfaces.
- **Registration**: provider key testing with capped protocol sweep.
- **Proxy**: attempt/commit split — combo failover that never leaks a failed hop.
- **Router**: combo name → ordered candidate hops; strategies.
- **Admin**: admin API, embedded dashboard, combo inference wiring.
- **Docs**: API reference, key-test design record.
