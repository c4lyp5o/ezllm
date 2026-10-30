# Changelog

Notable changes, reverse-chronological. Milestone-tagged (M1–M14), then by release; full history via `git log`.

## 2026-10-30 — the bridge CLI gate (v1.6.0)

- `ezllm bridge-check {claude|codex|agy} [--bin] [--home] [--json]` runs a
  bridge's boot checks against a staged CLI and refuses on any problem, the
  first milestone of unattended CLI updates (`SPEC-bridge-update.md`).
- Bridges read `qualified.json` from the bridge directory: a version the
  updater proved reports qualified. Claude's bridge status gains `cli_release`.
- `--live` adds real turns through the bridge's runner (plain, plus a client
  tool for Claude and Codex), only after every static check passed.
- `scripts/bridge_update.py` + systemd units: unattended bridge CLI updates
  (soak, stage, gate, promote in a window, prove, roll back), runbook in
  `docs/bridge-update-runbook.md`. First dry run on the box gated Claude
  2.1.308 and agy 1.4.11 ok and blocked Codex 0.166.0 on its lockdown check.
- The updater's reports reach the dashboard: `update` on `GET /admin/bridges`,
  a *CLI updates* line on bridge presets and Settings → System, and bell events
  `bridge_updated`, `bridge_update_blocked`, `bridge_rolled_back` (toggles
  `notify_bridge_*`, default on), each raised once per report.
- Codex lockdown accepts `worktrees` and `realtime_conversation` only at stage
  `removed` (retired in 0.166.0, traced in source to nothing the bridge reaches);
  the real 0.163.0 and 0.166.0 `features list` outputs are test fixtures.
  Codex 0.166.0 now passes the gate.
- Fixed: Claude boot logged `--max-turns` and `--system-prompt-file` as flag
  drift on every boot; the flags are now checked by the CLI's parser, not
  `--help`.
- Test: a Codex release that accepts but ignores `--disable shell_tool` fails
  preflight end to end.

## 2026-10-29 — a dashboard polish pass and the API it needed (v1.5.0)

- Every operator page was reworked in place: health and status where the
  operator looks first, deep links to the fix, dialogs that submit on Enter,
  lock while saving, explain bad input at the field and guard unsaved edits.
  Providers can add or replace a key from the Edit dialog; Chat gained Stop,
  answer actions, parameters, export and a ledger-backed "served by" line;
  Combos shows hop health, live block cursors and per-combo traffic; Rules and
  Connect gained validation with live readouts (what an allowlist matches, what
  a rule means); the Neighbourhood gained a usage window and full screen.
- `GET /admin/requests` filters by exact `account`, `model`, `alias`, `client`
  and `session`. `POST /admin/combos/{id}/test` accepts `hop`. Service cards
  list their `probes`. New `POST /admin/compression-preview` runs the engines
  over a pasted sample without storing or sending anything.
- Fixed: the dialog's "Keep editing" button submitted the form.
- Browser regressions: one new driver per page (`verify-*-qol.mjs`).

## 2026-10-28 — account safety, operational visibility and a living Neighbourhood city (v1.4.0)

- Provider-key retests and enable/disable operations now require the key to
  belong to the account in the URL. A mismatch returns 404 before a provider
  probe or key update.
- Gemini Web persists rotated cookies for matching stored keys, including normal
  routed requests; temporary probe credentials do not overwrite stored keys.
  A failed persistence attempt is warned and retried.
- Dashboard login and first-boot setup count in-flight attempts in the per-source
  limit and bound the source table. Ledger overload uses a bounded reservation
  cleanup queue; reservations that cannot be queued expire normally.
- The overview exposes failed ledger flushes alongside dropped and unmetered
  rows. Report and quota load failures now show an unavailable state and retry
  instead of suggesting that usage, cost or caps are zero. Failed background
  rules refreshes are logged while the last successfully loaded rules remain active.
- Chat cancels requests when their column is removed or the page is left, parses
  CRLF stream boundaries across chunks, and loads provider catalogs with bounded
  concurrency and partial-failure recovery. The mobile comparison view stacks
  columns; model pickers expose their name and highlighted option to assistive
  technology. Dialogs contain keyboard focus and restore it on close.
- Connect offers a copyable first request; bridge presets show tool availability
  and link to troubleshooting. Hidden dashboard tabs skip notification and
  backup polling. Economics applies its minimum-input filter on Analyze.
- CI compares the committed dashboard bundle with a fresh build, normalizing
  Vite's checkout-dependent asset hash suffixes. Browser regressions cover the
  dashboard changes; the short Go suite is the local functional gate.
- Neighbourhood is now a generated isometric city instead of a row of houses:
  one building per provider account on a seeded street grid, height and window
  light from tokens per minute, rain for failing providers, boarded windows for
  disabled ones, traffic density from the live request rate, and a flare plus a
  car at a provider's door for every committed call on `/admin/stream`. Canvas
  2D only, 60 fps on desktop, a still frame for reduced motion. `web/tests/`
  gains `shot-city.mjs`; `verify-neighbourhood*.mjs` now assert the city's
  contract (including that each tower paints where the plan says).
- The city breathes on its own: people walking the pavements (umbrellas up in
  the rain), cars drawn with cabin, glass, wheels and lights, a plane crossing
  the sky every minute or two behind the towers, and occasional rain showers
  that can also be commanded from a new Auto sky / Rain / Clear control. At
  night, lit buildings throw their windows down the street in front of them.
  The provider table gained a trend arrow for what moved since the last update.
  Two scene bugs found and fixed along the way: the animation clock never
  advanced (traffic lights, rain streaks and walk cycles were frozen), and a
  single early dispatched taxi stopped the scene from ever seeding its people.
- Providers: the Claude Pro/Max (claude-sub) account form now takes the
  claude.ai session cookie, so the quota panel reads the plan's 5-hour and
  weekly windows live instead of the bridge's last-observed reading. The read
  itself existed — it takes the cookie from the account's management key — but
  the form never offered a field to paste one. The edit drawer rotates the
  stored value the same way.

## 2026-10-27 — a cancelled caller gets the cancellation, not a parked hop error (fix)

v1.3.2 taught the dispatcher that a client hanging up must not charge a strike
against the hop. It did not make the request END as a cancellation. When the last
hop answers a failover status, that response is parked so it can be committed if
nothing else serves; if the caller had already left, the loop's context check
broke out without clearing the parked response, `commit` wrote the 503 and
returned a **nil error**. The cancellation reached neither the server nor the
ledger, and a hop error was recorded as the reason the call ended.

Whether it happened at all depended on which side won the race: a hop whose
transport noticed the cancel surfaced `context.Canceled`, while a hop that had
already written its 503 was committed as a success. It surfaced as a test
failing roughly one run in five — 9 failures in 50 local runs of
`TestClientCancelStopsTheFailoverLoop` — which is exactly the shape of a bug that
ships: too rare to block a green suite, frequent enough to corrupt the ledger's
account of why a request ended.

One branch at the loop head: a cancellation outranks a parked fallback. The body
is discarded, the error path answers, and an earlier transport error stays the
first cause. 200/200 after.

## 2026-10-27 — the released binaries finally have a manual (docs)

`release.yml` has shipped five platform binaries since v1.0.0 and nothing
explained how to run one — the README covered `make check` and Docker, and the
only macOS mentions under `docs/` were the Codex bridge refusing to start there.
[docs/install-binary.md](docs/install-binary.md) now covers download and
checksum (SHA256SUMS lists every platform, so a bare `sha256sum -c` fails on the
four archives you did not fetch), the mandatory `config.yaml` and the two flags
that override it, what the process writes, the first-boot setup code, a
throwaway-instance recipe for borrowed machines, and one service shape per OS:
a systemd user unit (with the lingering it needs to survive logout), a hardened
system unit, a launchd agent, and Windows Task Scheduler — because `ezllm.exe`
is a console application, `New-Service` fails on it, and the launcher has to be
a `.cmd` wrapper since Task Scheduler has no environment block.

`scripts/ezllm-mac.sh` is the macOS half, runnable end to end: architecture
autodetect (Rosetta included), checksum, layout, and an agent with the master
key pinned beside the data directory.

A third trap the doc now states: the provider accounts a "fresh" install already
shows are not auto-created by code — `seed()` upserts every entry under
`providers:` in the bootstrap config. Copying the v1.3.2 example therefore seeded
`opencode-main` and `ssn-gpt` as *enabled accounts with no key*, which is also
why the boot log warned twice about a failed model sync. `config.example.yaml`
now ships both commented out, so a fresh instance starts with zero accounts —
verified by booting it (healthy, 0 accounts).

Two more facts the doc states because they are traps rather than preferences: the
darwin/arm64 binary carries the Go linker's ad-hoc code signature (Apple
Silicon kills an unsigned one with `Killed: 9`), and `EZLLM_MASTER_KEY_FILE`
must be set explicitly — the default is home-global, so a second instance
silently shares the first one's key.

Verified: the install path executed against the real v1.3.2 assets on all three
platforms. **macOS:** the release binary was installed and ran on a Mac — the
ad-hoc-signed arm64 build executes without a `codesign` step, which is the one
thing the artifact alone could not prove. **Windows** 11 / Server 2025, build
26300: PowerShell download and `Get-FileHash` matched `SHA256SUMS`,
`Expand-Archive` + layout, a mandatory `config.yaml` failure with exit code 1, a
throwaway instance serving `{"status":"ok","version":"1.3.2"}` with the
dashboard at 200 and `/v1/models` returning 401 anonymous, the `.cmd` launcher
doing the same on a second port, and a clean stop leaving no process or listener
behind. One documented claim came back **wrong and is now corrected**:
`New-Service` does not fail — the SCM registers the console app happily and then
refuses to start it with `StartService FAILED 1053`, which is the real reason to
use Task Scheduler. Unexercised: the launchd agent registration
(`scripts/ezllm-mac.sh start`) and the Windows Task Scheduler registration — the
plist parses and the `.cmd` that Task Scheduler would launch is proven, and that
is the extent of the claim.

## 2026-10-27 — a history refusal fails over instead of ending the run (fix)

A thinking model that requires the reasoning of every earlier assistant turn
cannot serve a conversation another model started. It answers a 400 — and 400 is
not in the failover set, because a 400 normally describes the BODY we are about
to replay byte-identically. This one describes the HISTORY, so committing it
ended a client run that a hop of the same family could have served. Four such
rows in the ledger (`The reasoning_content in the thinking mode must be passed
back to the API`, and `Missing required parameter: 'messages[2].reasoning_content'
… needs the reasoning of every earlier assistant turn`), each one killing the run.

`refusesHistory` now recognises that class and the dispatcher fails over, exactly
as it does for a hop that cannot serve a wire. Deliberately **not** parked: the
hop is healthy for a conversation it started itself, so a cooldown would take a
good endpoint out of rotation for an unrelated reason. The 400 still reaches the
client when EVERY hop refuses, because the last response stays committable as the
fallback — the change turns a hard stop into "try the next hop", it does not hide
the refusal.

The cause is now visible as `[reasoning_echo_required]` instead of `bad_request`,
which is what lets an operator count the occurrences and fix the real problem:
a combo whose hops span model families. Under `bad_request` it read as a
malformed request and nothing pointed at the combo design.

## 2026-10-27 — the error table stops calling a bad body "capacity" (fix)

`ErrorCause` matched opencode's `Upstream request failed:` envelope before it
looked inside it, so every 4xx the edge wrapped was classified
`upstream_capacity` — "the backend was busy". The edge types those `server_error`
too, which made the misclassification look deliberate. Real bodies caught in the
ledger: `[missing_required_parameter] Missing required parameter:
'messages[347].content'`, `[bad_request_error] invalid params … requires adapter`,
and `[unsupported_value] Unsupported value: 'reasoning_effort'`.

The bracketed cause inside the envelope now wins, because it is more specific and
it describes the REQUEST, not the backend: `[missing_required_parameter]`,
`[bad_request_error]`, `[invalid_params]`, `[unsupported_value]` and
`[invalid_request_error]` all classify as `bad_request`. The genuine capacity
refusal (`Endpoint is unavailable`) still classifies as `upstream_capacity`, and
a test pins that case too — the point is the ordering, not a blanket rewrite.

Why it matters beyond tidiness: a request-shape bug labelled "upstream was busy"
is a bug nobody goes looking for, and the dashboard's cause chip told the
operator to wait rather than to fix the body.

## 2026-10-27 — a client hanging up no longer parks the hop (fix)

`classifyHopPolicy` classified a cancelled CALLER context as a transport error,
because a cancelled dial returns `context.Canceled`, which is a NetError. One
line, and the consequence is disproportionate: the hop banks a strike, the
default threshold parks it for the full five-minute cooldown, and the next real
request is refused from a local counter. The ledger says exactly that happened —
all 80 of the 502s in the previous seven days were `context canceled` (38 on
`daily`/anthropic), and eight later requests were answered "all combo endpoints
are in five-minute cooldown".

A cancellation is now a third outcome, neither success nor hop failure: it banks
no strike, parks nothing, and — importantly — does not CLEAR an existing park
either, since the abandoned request learnt nothing about the hop. The attempt
trail says `client cancelled: …` and logs at Info (a closed tab is routine, not
an incident), instead of a Warn "hop failed, failing over" that pointed the
operator at a healthy endpoint.

The ledger row is recorded as `499` with `client cancelled` in the error column
rather than `502 upstream_error`: the caller is gone, so nothing useful is
written to it, but the gateway must not report a fault it did not have.

Proved black-box, not just by unit: `TestClientCancelIsRecordedAs499AndDoesNotParkTheHop`
hangs the upstream, times the caller out mid-flight, asserts the 499 row, then
sends a REAL follow-up request that must be served — which is the only assertion
that proves the park is gone. `TestClientCancelDoesNotParkTheHop` pins the
dispatcher state directly. Both were bitten against their own producer and both
go red without the fix.

## 2026-10-27 — the reasoning clamp actually runs (fix)

The reasoning-effort clamp shipped in v1.2.0 was inert on every real request,
and the ledger proves it: 20 of the 29 `bad_request` 400s in the previous seven
days were `Invalid option: expected one of "off"|"low"|…`, and the log re-learned
the SAME vocabulary for the same model four times inside 45 minutes. Two bugs,
one behind the other.

**The rule never reached the route.** `ResolveCandidates` swaps the catalog for a
`*store.CatalogSnapshot`, and `hopFacts.reasoning()` reads the rule out of
whatever `ModelFacts` it is handed. The snapshot's model query selected the pin
and the three protocol flags but never `reasoning_vocab` / `reasoning_required`,
so it built `ModelFacts` with a zero rule. The direct `*DB.ModelFacts` read had
the vocabulary — which is why the store tests passed while production never saw
a rule, every request went out unclamped, and each refusal re-taught the same
list. The snapshot is a second projection of that row and now carries the same
columns.

**A required-reasoning model could still be sent a disabling effort.** Command
Code enumerated `off|low|medium|high|xhigh|max` and then refused `off` for one
model — "Only models that can turn thinking off accept it." The vocabulary and
the mandate arrive as separate facts, so `Clamp` picked `off` as the cheapest
allowed value and turned a bogus client value into a guaranteed 400 that looked
like a correct clamp. `Required` now governs the selection: a rank-0 value is
never sent, and when nothing at or below the ask is legal the field is DROPPED so
the provider's own default applies — inventing a higher effort would buy
reasoning the client did not ask for. The vendor's wording is now in the phrase
table, so the refusal teaches `Required` instead of being discarded.

Proved end-to-end on a canary rig (copied prod database, current-tree binary,
loopback port, real provider, `max_tokens: 5`): a bogus effort returned 400 →
`reasoning vocabulary learned … allowed=""` → the retry DROPPED the field and
returned 200, twice. The stored row now reads `reasoning_required=1`.

Coverage added at the seam that broke: `TestCatalogSnapshotCarriesReasoningRule`
(snapshot parity), `TestResolvedRouteCarriesLearnedReasoningRule` (a resolved
Route carries the rule — every existing clamp test built its Route by hand with
the rule attached, which is exactly why the hole survived),
`TestClampNeverSendsADisablingEffortWhenRequired`, and
`TestParseReasoningRequiredFromThinkingOffRefusal`.

## 2026-10-27 — github-copilot wire guard and persona stealth (behaviour change)

Resolves the three findings the previous pass deliberately left unfixed, because
each one changes what the running gateway does. Two are behaviour changes on the
github-copilot surface; one is a test that could never have failed.

**Persona stealth now actually runs.** The "stealth" rule — drop `role:system`
turns so the client's persona and personal context never reach a web provider —
was documented but only reachable through `DoChat`, which was linker-dead. It
was carried in the body-quirk pass (`PrepareBody`) the live adapter calls, so
every github-copilot request now sheds its system turns. This is deliberate
output loss: a request whose only content was a system prompt leaves with an
empty `messages` list and the upstream's own "empty messages" error is the
honest answer for a call that carried nothing but persona. If you want system
prompts to reach Copilot, this is the change to revisit.

**Gemini/Claude no longer dialled on /responses.** The old endpoint router
carried a hard rule — claude* is Anthropic-native, gemini* rides
/chat/completions, and neither answers on the Responses wire (400
`unsupported_api_for_model`). That rule died with `endpointForModel`, so a
claude/gemini request on the responses surface went out on a wire the model
cannot speak and returned a mystery 400. The github-copilot adapter now refuses
before dialling and types the refusal as `ErrWireUnsupported`, which the
dispatcher treats as a hop fact: it fails over to a hop that can serve the wire
instead of committing the refusal. Deliberately not parked — the park key is
account+base+model and cannot tell wires apart, so parking would stop the same
model being dialled on the wire it *does* serve.

Copilot's own phrase `unsupported_api_for_model` was also added to the
capability tables (`refusesWire`, `ErrorCause`), so an upstream answer of that
shape is diagnosed as `protocol_unsupported` and fails over rather than being
reported as a bad request. Defense in depth for model names the rule does not
recognise.

**A test that could not fail was replaced with one that can.**
`TestCaptureShapeDocParityWithBytePath` compared `captureShape` against
`captureShapeDoc(...)` — but `captureShape` *was* that delegation, so it
compared a function with itself. It is replaced by `TestCaptureShapeDocContract`
(assertions over the body corpus: what hashes, what does not) and
`TestCaptureShapeDocHashScope` (the cache-reuse invariant: appending a turn must
break the prefix hash, editing the system prompt must, swapping only the newest
turn must not).

## 2026-10-27 — dead-code removal and comment correctness (no behaviour change)

Maintenance pass with no functional change: removed 32 unreachable functions,
moved test-only constructors out of production files, and corrected comments
that described code which no longer exists. The shipped binary is byte-for-byte
the same size (18,481,312) and its symbol set is unchanged (20,811 symbols,
none added, none removed) — the deleted code was already pruned by the linker,
so routing, translation, compression and accounting behave exactly as before.

Every removed function was classified first. Pure orphans went with their
tests (`storeSurface`, `CavemanDiags`, `proxy.StatusText`/`truncate`, and the
`pickProbeModel` assertion that only restated `pickProbeModels()[0]`). One-line
wrappers over a live core went with the wrapper, and any test that asserted
LIVE behaviour was repointed at the entry production actually calls rather
than dropped: the legacy `CodecOptions{Legacy:true}` translation cells now
exercise `ResponseUniversal`/`NewStreamReader` directly (23 call sites), the
usage normaliser calls `NormalizeUsageWith` (7), the compression engines call
`ApplyWithSession` (42), and the park policy calls `classifyHopPolicy` (24).
`TestPollDeviceState` was upgraded in passing: it now asserts `slow_down` as a
DISTINCT state, the contract the dashboard device flow backs off on, instead of
the folded boolean the removed wrapper returned.

Comments were corrected where they were wrong, not merely trimmed: the claim
that `EncodeRequest` was "the one entry point the dispatcher uses", that
`NormalizeUsage` was the normaliser, and that `CavemanDiags` was read by "the
admin surface" were all false. Durable contract text was moved onto the
function that now owns it (the named-return/MS-stamp trap to `applyPipeline`,
the per-surface usage map to `sseUsageRaw`, the park-duration precedence to
`classifyHopPolicy`), and two stale line-number references were replaced with
symbol references.

`make build` now produces what ships: `CGO_ENABLED=0 -trimpath
-ldflags="-s -w"` (26.6 MB → 18.5 MB), with a `build-debug` target retaining
the symbol table.

Two findings came out of the pass and are NOT fixed here, because fixing either
would change behaviour: the github-copilot "stealth" filter (drop `role:system`
so persona/context never reach a web provider) existed only in the dead
`DoChat`, so it does not run today; and the "Gemini/Claude must never go to
`/responses`" routing guard existed only in the dead `endpointForModel`, the
live adapter routing by surface instead. A third, `TestCaptureShapeDocParityWithBytePath`, was deleted as unfalsifiable — it compared `captureShape` with
the `captureShapeDoc` call that `captureShape` delegated to.

## 2026-10-26 — v1.3.1: settings administration and encrypted recovery

Expanded Settings into General, Security, Backup & Recovery, Gateway,
Notifications and System. Operator settings persist in SQLite with revision
checks and explicit configuration provenance; deployment overrides remain locked.
Request policy snapshots preserve active retries/routing while new requests use
saved controls. Browser theme, reduced motion and analytics defaults remain local.

Added opaque dashboard session management, instance-wide notification preferences,
and allowlisted diagnostics. The healthz contract is unchanged. Model rules can
inherit the instance timezone while existing explicit boundaries are preserved.

Encrypted online backups include WAL data and the active master key, verify an
independent decrypted copy before publication, and begin retention only after
successful publication. Optional scheduling uses a mounted destination and a
separate backup secret. Offline verification and restore reject corrupted archives and
existing output directories; restored dashboard sessions are revoked.

Schema v31 adds settings, job-health and session-detail storage. Legacy empty
rule timezones migrate to explicit UTC; existing explicit zones and client-token
budget boundaries are preserved. Deployment-controlled values remain read-only
in Settings. Changing ordinary runtime policy affects newly admitted requests;
already-running requests retain their original snapshot. Export format version 1
is preserved with an additive non-secret settings field.

The login railway restores its busier glowing rails and fixes the small-screen
30 fps first-frame stall that left the canvas blank in v1.3. Packet interpolation
stays inside each segment, and OS/browser motion changes are applied live.
Neighbourhood now uses layered facades, gardens, streetlights and traffic, with
keyboard/touch selection and a numeric house inspector. Missing usage is shown
as unavailable; loading/errors support recovery, and wide streets scroll locally.
The SVG scene remains dependency-free and pauses independently of data refresh.

Provider presets and the bridge API use Google Antigravity, Claude Pro/Max,
ChatGPT Plus/Pro and OpenAI Platform. Newly added accounts use those names;
existing account names and routing identifiers remain unchanged.

## 2026-10-26 — v1.3.0: chat wire rename, probes out of the ledger, Antigravity bridge, one-click bridges

- **The chat-completions wire is named `chat`, not `openai`** (schema v30,
  breaking). The ledger `surface`, model pins, `protocol_support`, the
  `/admin/models` capability keys and the `proto_openai` column all become
  `chat` / `proto_chat`. The migration renames the column and rewrites stored
  pins and existing ledger rows in place; it is idempotent. `PUT .../pin` and
  the combo-test `surface` still accept `openai` as a legacy alias on input.
  Provider kind names (`openai-compatible`, `gemini-openai`) are unchanged.
- **Probes are no longer written to the ledger.** Key tests (`admin:test`),
  protocol sweeps, combo tests (`combo-test`) and service tests
  (`service-test`) leave no row; the verdicts they produce are still stored.
  Rows already in the ledger are untouched. This reverses the earlier rule
  that probe spend must be visible in the ledger.
- **Claude subscription quota can be read live.** Pasting the claude.ai Cookie
  header into the claude-sub account's `management_key` makes the Quota button
  read `claude.ai/api/organizations/{org}/usage` (five-hour and weekly
  windows) instead of the bridge's last-observed reading. The dialog adds the
  Opus/Sonnet windows and the per-product split of the week, names the source,
  and states why it fell back when the live read failed.
- **Antigravity subscription bridge.** A third loopback bridge
  (`EZLLM_AGY_BRIDGE=on`, `127.0.0.1:8792`) puts a personal Google Antigravity
  plan behind an `openai-compatible` account through the official `agy` CLI
  (1.4.7, headless stream-json, one process per request): Gemini 3.8 Flash and
  the Claude and GPT-OSS models the plan includes, served from what
  `agy models` lists at boot. Text chat with usage (1.4.7 sends the answer as
  one chunk at the end of the turn); client tools and images are refused, not
  dropped. The login is made with D-Bus unset so it is a token file the
  container can mount, not a desktop-keyring entry. The agent's own tools stay soft-denied: a turn whose CLI reports a looser
  permission mode is killed, and a login whose `settings.json` loosens the
  policy fails the boot. Google's "account not eligible" refusal becomes 403
  `account_ineligible` with its sign-in link withheld. Dashboard preset
  **Antigravity subscription (bridge)** lists the plan's models. Decisions:
  `docs/antigravity-subscription-design.md`; setup: `docs/agy-bridge-runbook.md`.
- **Subscription bridges in one click.** Add provider has a preset per bridge (**Claude
  subscription**, **ChatGPT subscription**, **Antigravity subscription**). Each shows
  whether its bridge is running (CLI version, login, plan or tool state) or the
  exact steps to turn it on, fills in name, base URL and usage convention, and
  needs no key: the add names `bridge` and the server supplies the shared secret,
  so nobody copies `data/*-bridge.token` off the host. New `GET /admin/bridges`
  (the secret is never in it); a key test against a bridge that is not running
  now names the variable that fixes it. Both runbooks open with a five-step
  quick setup and say how to install each CLI.
- **Lighter login backdrop.** At most 12 packets on a desktop and 5 on a phone
  (was 24), no per-packet `shadowBlur`, and phones draw at 1.5x density and 30
  fps, at the same on-screen speed.
- **Neighbourhood page (dashboard).** Every provider key is a house whose windows
  glow with its tokens per minute over the last five minutes; smoke means it is
  serving traffic, a rain cloud means a quarter or more of its recent calls
  failed, dark windows mean it is disabled. SVG and motion only, polled every
  10 s, with an exact-figures table underneath (glow meter per key; hovering a
  house highlights its row and back), a glyph legend, an "updated Ns ago"
  indicator, boarded windows on disabled keys, and a sun/moon. The one scene exempt from the
  no-glow rule — fenced in `web/DESIGN.md`. The Playwright harness's unlock
  marker was updated to the session marker the app has used since v1.2.0.

## 2026-10-24 — v1.2.0: reasoning-effort clamp, model-access park, logging

- **Reasoning effort follows the model's own vocabulary** (schema v29). A 4xx
  that enumerates accepted `reasoning_effort` values, or says reasoning cannot
  be disabled, is stored per account and model; later requests are clamped
  down into it instead of refused. Key-test probes learn it too, and an
  invalid-effort 400 no longer records a wire as unsupported.
- **A 403 that names model access parks that model on that hop** for the
  normal cooldown when another candidate exists, so a block of requests stops
  re-dialling it and losing its prompt cache to a fallback.
- **The per-key model allowlist fails closed.** A store error now answers
  `503 authorization_unavailable` instead of suspending the allowlist.
- **Key tests:** Copilot credentials refused at token exchange count as proven
  discrimination; an endpoint-mismatch refusal is re-probed on `/responses` and
  the wire recorded; failed probes write their reason into the ledger `err`.
- **Leaks:** parked fallback responses and failed Gemini audio rewrites close
  their upstream bodies on every exit.
- **Dashboard:** the Connect page reads `GET /admin/routable-models` through
  the session; the unused localStorage bearer path is gone.
- **Console logging says what it knows.** The access line names the requested
  target (`alias`) and carries the failover trail when a candidate did not
  serve. Auth refusals — which reached neither the ledger nor any log — are
  visible at `debug`, and the client-budget refusal logs once per window at
  `info`. The reasoning clamp now reports both values (`none` → `minimal`) and a
  mandatory-reasoning drop as a drop; a learned vocabulary and a wire recorded
  after an endpoint-mismatch probe are reported at `info`. The boot states the
  effective level. `EZLLM_LOG_FORMAT=json` for collectors; stdout only, no log
  file. The contract and the measured cost are in
  [docs/logging.md](docs/logging.md).

## 2026-10-23 — v1.1.1: audit fixes — security, compression sessions, dashboard, recovery, performance

- **Same-origin cookie writes.** A signed-in operator's browser sends the
  `SameSite=Strict` session cookie to every origin of the same site, and a
  `text/plain` POST needs no preflight, so a page on another port could mint an
  admin token. Cookie-authenticated writes and sign-out now pass
  `http.CrossOriginProtection` (`cookieOrigins`) or get `403`; bearer and
  non-browser clients are unaffected.
- **No cross-origin redirects with provider keys.** Go drops `Authorization` on
  a cross-host redirect but keeps `x-api-key`, and a 307 replays the prompt.
  The upstream client and the key tester now use `provider.SameOriginRedirect`.
- **Compression sessions are per client and honest about their version.** An
  explicit session is scoped to the authenticated client. A `tool_ref`
  reference is replayed only while its target still holds the same output. A
  bound session runs its bound profile snapshot, not just its version stamp.
  The memo has per-session and global byte budgets and an idle sweep. Tool
  outputs are classed with names from the whole request, so a file read after
  a memoized call stays byte-identical. Caveman lite lost the rules that
  changed quantity, time or certainty.
- **Dashboard fixes**, each with a Playwright regression driver on the new
  `web/tests/mockdash.mjs` harness:
  - Policy saves no longer move the key's expiry.
  - Pricing saves work.
  - Chat surfaces stream errors.
  - Analytics ignore stale responses.
  - Grouped rule writes reconcile after a partial failure.
- **Metering, pricing and recovery:**
  - USD-only rates.
  - Source-wire usage capture.
  - Retries and failed hops are ledgered.
  - Missed meter debits are replayed (schema v28).
  - Master keys are preserved.
  - Atomic legacy combo rebuild.
  - Verified WAL-safe backups (`scripts/backup_sqlite.py`).
  - Versioned configuration exports.
- **Docs pass:**
  - Landmarks cite symbols instead of line numbers.
  - The README, AGENTS, CONTRIBUTING and EXPLORING claims are re-verified
    against the code.
  - The translation plan is reduced to a design record.

- **Audit fixes** (the first pass on `fix/audit-findings`):
  - The Claude web provider verifies TLS and opens one upstream conversation
    per request.
  - Rules retry after a failed boot load.
  - The Gemini web client resolves under the per-account lock.
  - Strict JSON body endings.
  - Shutdown drains handlers and background jobs before the ledger closes.
  - Bridge children end with their request.
  - Budget reservations are released by the flush that counts them.
  - The CSP admits generated media previews.
- **Performance pass:**
  - O(1) memo eviction.
  - Streamed shape hashing (1 MiB history: 1.4 KB allocated instead of
    1.9–3.5 MB).
  - A faster local-timezone day report.
  - 10 s shared usage reports.
  - Preallocated body reads.
  - One HTTP/2 connection for Claude web.
  - Lazy dashboard pages (290 KB → 124 KB gzipped at sign-in).

- **Bound Codex output buffering:** a pending queue holds at most 1 MiB or
  4,096 deltas, in addition to the batch currently being delivered. Overflow
  fails with `output_backpressure` and independently reaps the child; the stdout
  reader never waits on a stalled client while holding a state lock.
- **Fix Claude web transport cleanup races:** closing a fetcher uses the same
  initialization barrier as its first request.
- **Keep report-cache errors and filters honest:** a completed failed computation
  cannot become a successful nil result, and explicit `from` boundaries no
  longer collide after rounding into the same ten-second interval.

## 2026-10-23 — v1.1.0: ChatGPT subscription bridge, plan windows, probe and Copilot fixes

- **ChatGPT subscription as an account** (`internal/codexbridge`, off by default).
  The official `codex app-server` sits behind a loopback OpenAI-compatible surface
  (`127.0.0.1:8791`); every child runs with Codex's own shell, patch, web and
  update features disabled and a read-only sandbox, so it executes nothing of its
  own. Client tools travel through code-mode and come back as `tool_calls`; the
  child is parked until the results arrive (continuation), and every child is
  confined by Landlock — without Landlock, tools are refused unless
  `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK=on`. Structured output, reasoning summaries
  as `reasoning_content`, all models including hidden ones, and a `codex exec`
  fallback when the app-server lacks a required method
  (`docs/codex-bridge-runbook.md`, `docs/codex-subscription-design.md`).
- **Plan windows on the provider card.** The Claude bridge serves the CLI's 5h/7d
  `rate_limit_event` on `/v1/usage`, the Codex bridge serves `account/rateLimits/read`
  in the same shape; ezllm reads it for loopback accounts.
- **Fix: key test advances past endpoint-mismatch models** ("not accessible via
  /chat/completions") and no longer blocks a proven key on a non-auth generation
  failure.
- **Fix: Copilot presents CLI 1.0.103.** GitHub's HTTP 466 floor rejected 1.0.81-6;
  compose passes `GITHUB_COPILOT_CLI_VERSION` through.
- **Deploy change: one compose file, yours.** `compose.yml` is no longer shipped:
  copy `compose.example.yml` (every variable with its default, bridges off, mounts
  commented) to `compose.yml`, which is gitignored. `compose.traefik.yml` is gone;
  proxy labels go in your copy. **Before pulling,** back up your `compose.yml`:
  git deletes a formerly tracked file on pull.

## 2026-10-22 — v1.0.0: one translation switch, env reference, probe fixes, arm64 image

- **Behaviour change: `translation.enabled` is the one switch.** It now covers the
  pairwise translators and the universal matrix together. `translation.universal_enabled`
  and `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` still work but warn at boot; an explicit
  `universal_enabled: false` now turns all translation off, not just the matrix
  (`docs/translation-runbook.md`).
- **Every environment variable documented.** `docs/environment.md` lists each one
  with its default and opens with what a new install sets (`EZLLM_USAGE_TZ`, the
  first dashboard password, `EZLLM_CLAUDE_BRIDGE=off` without a Claude CLI). The
  duplicate Claude bridge defaults left `compose.yml` and the `Dockerfile`; the
  binary's own defaults (`/opt/claude/claude`, `/claude-sub`) are unchanged.
- **Shipped compose still turns the Claude bridge on**, with paths for this
  maintainer's host. Installs without the Claude CLI and a subscription login set
  `EZLLM_CLAUDE_BRIDGE=off`, or the boot fails.
- **Fix: a retired model no longer fails a key test.** Ollama answers a retired
  model with `410 Gone` ("… was retired on …"); the probe now treats it like
  Gemini's retired-model 404 and moves to the next candidate.
- **Fix: Ollama (local) keeps the operator's base URL**, so a remote Ollama host
  is no longer replaced by `localhost`; quota percentages render whole.
- **Fix: the models drawer refreshes after a catalog sync** without a page reload.
- **arm64 image again.** `ghcr.io/c4lyp5o/ezllm` is built for `linux/amd64` and
  `linux/arm64`; the Dockerfile cross-compiles on the runner (`GOARCH=$TARGETARCH`),
  so arm64 adds no emulated build time.
- **Semantic versioning from v1.0.0.** A release tag must match
  `internal/server/version.go`; both release workflows refuse one that does not.
- **Docs accuracy pass.** README Security now states keys are AES-256-GCM
  encrypted at rest (it said plaintext); EXPLORING.md landmarks re-verified;
  resolved known issues removed from CONTRIBUTING.md.

## 2026-10-21 — Claude subscription M2: tools on claude-sub

- **Agents can run on the subscription.** Hermes sends tools on every turn, so M1's
  blanket refusal answered every agent message with a 400. Client `tools` and
  `tool_choice` now travel in `CLAUDE_CODE_EXTRA_BODY`; upstream sees exactly the
  client's tools and the `tool_use` blocks come back under their names
  (`docs/claude-subscription-design.md` §2.1, §6).
- **Measured, not assumed (CLI 2.1.306).** A live spike replaced the planned inert
  MCP server: one generation per call, a `write_file` sentinel never written, and
  Hermes's real 57-tool list cached normally (24,112 tokens written once, then read
  on every later turn).
- **Native's quirks stay inside the bridge.** Its synthetic "No such tool"
  `tool_result`, the `error_max_turns` ending, the empty-reply nudge (a second
  message), and its one-frame-per-block output are all handled: the client sees one
  well-formed message, with every parallel call.
- **Cache breakpoint budget.** Native spends two of upstream's four
  `cache_control` slots; four client markers were a live
  `400 … Found 6`, so the newest two are kept.
- **Drain cap through Rules.** sonnet/haiku 300,000 tokens per 5h, opus 1, set
  before an agent is moved onto the combo (runbook).
- **CLI pin survives updates.** 2.1.306 is pinned as a copy outside the
  auto-updater's directory, which had deleted 2.1.299 under the live mount.

## 2026-10-21 — Cloudflare free models in auto-free combos

- **Built-in Cloudflare free list.** `internal/provider/cloudflare_free.go`
  names the 65 Workers AI models the dashboard shows free, with their task.
  `syncAutoFree` adds the chat-capable ones (`Text Generation`) when the
  account is a host-verified Workers AI account (`api.cloudflare.com`,
  `/ai/v1`); the other classes are data only — a chat combo cannot serve an
  embedding or an image model, and catalogs do not advertise capability
  (`docs/services-design.md`).
- **Five more service endpoints.** `/v1/images/edits` (multipart, shares the
  STT handler), `/v1/moderations`, `/v1/rerank`, `/v1/videos/generations`,
  `/v1/music/generations` — routed, ledgered under their own surface,
  pinnable, with Services cards and probes (paid ones confirmed first).
- **Fix: service surfaces never read the chat probe column.** `proto_openai`
  was consulted for every non-chat surface, so a model that failed the chat
  probe was refused on `/v1/embeddings` & co.
- **Fix: service-surface pins and multipart refusals.** `surfaceName` named
  every service surface "openai", so an `embeddings` pin refused
  `/v1/embeddings` and an `openai` pin admitted `/v1/rerank`; pin translation
  is now chat-wire only. The multipart handler shares `writeResolveErr`, so
  `pinned_surface` / `wire_unsupported` are 400 there too, and the ledger
  records `wire_unsupported` as 400 rather than 502.
- **Services "Try it".** `POST /admin/services/{surface}/generate` runs the
  operator's model-less request through the normal pipeline with the saved
  default (like `/admin/chat`); the card renders images, audio, video,
  transcripts or the raw JSON. Video/music have no common upstream format,
  so only the prompt is sent and async job answers are shown as they come.
- **Local Ollama preset.** Ollama serves both wires natively
  (`/v1/chat/completions` and `/v1/messages`); the preset only fills the
  base URL.

## 2026-10-20 — ten compression engines, universal translation, data-terminal dashboard

- **Auto-free combos (schema v25).** `combos.auto_free` makes a combo's hops
  the free-marked models (`:free` / `-free` ids) of the account catalogs,
  kept in sync on every catalog write, account enable flip and combo save;
  `auto_free_accounts` narrows the sources and `auto_free_include` pulls in
  unmarked promos such as Ollama's `beam`. Fresh databases are seeded with
  an empty `ezllm-auto-free` combo that fills as providers are added.

- **Compression: ten engines on one wire-neutral text walker.** New: `ansi`
  and `fold` (terminal output), `jsonmin` (whitespace-free JSON tool output),
  `tool_ref` (repeated tool results become references), `blobs` (base64 runs
  and data URIs become markers). The `budget` engine is deleted — it needed a
  known `max_tokens` and did nothing without one. A request is transformed
  identically whether it arrives on chat, messages or responses.
- **The pipeline is ordered by meaning.** A per-profile `tool_classes` table
  names what each tool is (file read, edit, diff, …); file reads ship
  byte-identical so the agent's next edit still matches; an unknown tool is
  its own class — never truncated, only tidied by the lossless engines.
  `fresh_tool_output` decides whether a purely-tool final message may be
  trimmed on its first send (schema v24). A committed-prefix memo keeps the
  cached prefix exactly equal to what was committed.
- **Dashboard**: the `/admin/usage` totals row now carries `applied_saved`
  (Stats' "Removed by compression" card read 0 over non-zero rows); the
  budget-stage box, the "context saved" column, the version in the Dashboard
  strip and the "(unconfirmed)"/"(no probe)" surface tags are gone; Chat opens
  with one column.
- **Universal wire translation is default-on** — no yaml needed, and an
  anthropic-only account is pre-marked so the universal matrix translates
  instead of refusing. One translation path in `commit`.
- **A hop that refuses the WIRE now fails over.** An upstream 400 that names a
  wire-protocol gap (`ModelProtocolUnsupported`, `not supported for format`)
  describes the HOP, not the body, so the next hop serves the identical
  request and the refusing hop is parked. Ordinary 400s still pass through
  unchanged, and a sole hop still returns its own 400 verbatim. Motivated by
  `daily-pro`: the opencode-go hop refused `/v1/messages` while the command-code
  hop was never tried.
- **Combo hop auto-probe.** A combo write (`POST /admin/combos`, `PATCH`,
  `POST|PUT …/hops`) starts a background protocol probe of that combo's
  ENABLED hop models only — never the account catalog — on the surfaces still
  lacking a verdict. An account sweep samples a 500+ model catalog, so the hop
  an operator actually routes through was often never sampled; curated rows,
  disabled hops and disabled combos cost nothing.
  `EZLLM_COMBO_AUTOPROBE=off` disables it.
- **Proxy**: every input token lands in one billed bucket, the account usage
  convention is honoured, the hop model is spliced into the client bytes
  instead of being re-marshalled, a hop stays parked after a transport error,
  and an oversized body is a 413.
- **Provider/registration**: quota and catalog reads honour custom headers,
  pagination and no-endpoint statuses; the management key stays on
  tokenrouter; retries honour http statuses; sweeps keep proven verdicts; a
  claude-web key keeps its wire.
- **Perf**: one catalog read per hop; an in-memory routing catalog snapshot
  invalidated by `catalog_gen` (schema v23); a throttled reservation prune;
  an allocation-free header strip.
- **Dashboard: the data-terminal redesign.** Exact numbers (never
  abbreviated), motion.dev throughout, and a screen-by-screen polish pass
  verified in a live browser — including three fixes the pass surfaced: the
  modal's exit animation was unreachable, a page-level `AnimatePresence
  initial={false}` was suppressing every descendant entrance, and the
  committed bundle was carrying dead CSS classes because Tailwind was
  scanning the tracked `web/dist/` output. The build is now reproducible.
- **Dashboard: the roster and rules re-read.** The Providers page is a roster
  — one expandable row per account with a fixed-width quota column, so every
  figure sits on one right edge instead of ragged per-card alignment. Rules
  are one table row per SETTING rather than per model: rules on the same
  provider with the same cap, window, state and note collapse into one group
  listing its models (the wire stays one rule per account+model — a group is a
  view, not a schema change). The Dashboard's health strip becomes a header
  pill: an uptime indicator, with the account/model/combo counts gone from
  it because inventory is not health and those counts live on their own
  screens. Labels are sentence-case throughout.
- `unmetered_rows` on health, overview, live feed and dashboard; the tz day
  report's `applied_saved`; the meter is all-or-nothing per batch; web
  bridges require a process secret.

## 2026-10-19 — claude-bridge M1: the Claude subscription as an ezllm account

The Claude subscription becomes an ezllm account without the router learning a new
trick. A **loopback-only surface inside the ezllm process** serves an Anthropic
Messages shape by driving the **official Claude Code CLI** — bind-mounted from the
host — against a subscription login, and ezllm dials it as an ordinary
`anthropic-compatible` account. The CLI stays the only thing that speaks to the
subscription — the credential never leaves its own login store — which is the
whole point: no OAuth-bearer reuse, no client impersonation
(`docs/claude-subscription-design.md`, locked decision 1).

- **Request path.** The client's Messages body is handed to native through
  `CLAUDE_CODE_EXTRA_BODY` (the mechanism the official Hermes plugin documents),
  `system` travels via `--system-prompt-file`, and replayed history frames carry
  `shouldQuery:false` so native acknowledges them with a zero-turn result instead of
  generating — and charging the plan — once per frame.
- **Measured, not assumed.** With the body carrying history and the stdin frames
  carrying a *different* history, the answer followed the body: the injection is the
  upstream request. One generation per call. And the CLI's own ~20k-token harness
  overhead is gone — a three-message body measured 170 input tokens on the wire.
- **M1 is text-only by declaration.** Tool-bearing requests are refused with
  `chat_tools_unsupported` before a process is spawned; tools arrive in M2 through an
  inert MCP server (advertise, deny execution, capture the call).
- **Failures stay legible.** Native error codes map to honest statuses
  (`rate_limit`→429, `billing_error`→402, `authentication_failed`→401,
  `overloaded`→529) and the CLI's own words ride along unrewritten. A client's own
  `max_tokens` ceiling is a 400, never a retryable 5xx.
- **Key test, operator's call.** The generation probe is **skipped** for this
  account (`skip_inference`) — it is official Anthropic, Messages-only, and the
  credential is already proven by the auth oracle (a fake key is rejected).
  Inference evidence comes from the live proof instead of a "ping".
- **Probe budget (the NIM lesson, again).** The remaining probes budgeted 8–16
  output tokens, which a mandatory-thinking model (Haiku 5.5) cannot answer in —
  measured `max_output_tokens` failures on a perfectly good key. Probes now budget
  256 (`internal/registration/probe_body.go`), with a test that fails if anyone
  shrinks it.
- **Proof.** `tests/prove_claude_bridge.py`, through ezllm's front door against a
  real Pro subscription: **7/7** — completion, native usage fields, conversation
  history fidelity, tool refusal, and the ledger rows.
- **Container.** One container, `distroless/base` runtime: the CLI is glibc-dynamic,
  and on `/static` the exec fails with a misleading "no such file or directory"
  (observed live); Alpine is musl and cannot run it at all. The CLI binary mounts
  read-only and pinned by path, the login directory read-write (the CLI refreshes
  its own token), the account secret auto-generates into `/data` like the master
  key, and the surface binds loopback only — it refuses any other bind.

## 2026-10-18 — security milestones, native surfaces, playground, per-key budgets

- Security Milestone A: provider keys encrypted at rest (AES-256-GCM), master key, migration + tests.
- Security Milestone B: httpOnly cookie session, per-IP login rate limit, frontend cookie auth; cookie is `Secure` when the proxy reports `X-Forwarded-Proto: https` and scoped to `/admin`.
- Native image/audio adapters (Cloudflare, Google AI Studio); per-account management key (schema v19) for TokenRouter balance.
- Price catalog: `GET /v1/prices`, bulk import, and daily LiteLLM refresh; rates carry `source`/`fetched_at` (schema v22).
- Chat playground (1-3 columns), per-key budget meter + editor in Connect; budgets are now enforced (schema v20 `client_usage_counters`, 429 before dispatch, backfill for caps set after spend), with `max_tokens` reservation, per-key model allowlists, and key expiry (schema v21).
- Dashboard: routing-rescue panel; Linear-inspired palette and sidebar icons.
- UI: Providers/Services redesign, Connect leftover localStorage removed.
- Tests + docs updated; security audit report.

## 2026-10-17 — release versioning, docs sync, test-file housekeeping ✅
Version plumbing and a full documentation sync pass (release **v0.0.12**,
see `VERSION.md`):
- **`GET /healthz` now reports the release version**: new single source of
  truth `internal/server/version.go`, served as `{"status":"ok","version":…}`
  (503 down-payload carries it too); `/admin/overview` health object gains
  `version`, rendered as the first chip in the dashboard health strip.
  Guards: `version_test.go` fails on VERSION.md ↔ version.go drift and on a
  healthz payload that grows beyond `{status, version}`.
- **VERSION.md** (new): the release history — one version per change date,
  patches on the same date grouped under one entry, back-filled v0.0.1
  (2026-10-06) → v0.0.11 (2026-10-16) from `git log`.
- **Docs rewritten/synced**: README rebuilt as the project front door (no
  personal deploy details; version/healthz contract updated), CONTRIBUTING.md
  (new — workflow, test placement rules, memory findings, known issues),
  AGENTS.md (new — agent/maintainer hard rules, layout map),
  EXPLORING.md landmark re-verified against code (`RecordCall`, model-pin
  handler/route, M1 block line numbers, `Auth` interface), API.md healthz row
  + health object example refreshed from the new contract, design-doc
  citations corrected (`ValidateAccountInput`, `main.go` WriteTimeout).
- **Test-file housekeeping**: Python live proofs `scripts/prove_*.py` →
  `tests/`; Playwright drivers `web/{shot,verify}-*.mjs` → `web/tests/`. Go
  `_test.go` files intentionally stay in-package (toolchain constraint —
  CONTRIBUTING explains why).
- **Makefile**: `test` now runs `-count=1 -timeout 15m` (CI parity; cached
  results had masked changes), new `test-short` target for 4 GiB cgroups, and
  an honest comment on what GOMEMLIMIT does/doesn't cap (measured: soft heap
  limit ≠ RSS cap; full suite peaks ~4.21 GiB in the percentile harness).
- **CI**: `release.yml` tag builds now call the reusable `test.yml`
  (`needs: test`) — same gate as `docker-publish.yml`, so a red suite never
  produces an image or a release asset. CONTRIBUTING known-issue #1 closed.
- **Provider credential entry fixed (the "waiting for auth" stall)**: GitHub
  device-flow `slow_down` is surfaced (`GHDeviceStatus.slow_down`) and the
  dashboard backs its poll interval off per RFC 8628 §3.5 — it was folded into
  plain `pending`, so GitHub withheld the token forever while
  `github.com/login/device` already showed Success (reproved live: GitHub does
  send `slow_down`, 6/8 polls at 1s; its own advertised 5s is clean). Poll and
  mint now run on `context.WithoutCancel`, so a tab reload/blur mid-mint can no
  longer cancel an already-granted authorization and poison the handle;
  5xx/429/context errors classify retryable instead of terminal; the pending
  panel renders `detail`; every status failure reaches `ErrBox` (a non-ApiError
  used to be swallowed, leaving "Waiting…" with nothing under it); the flow now
  logs start/poll/authorize/refusal/expiry (8-char handle prefix only — the
  full handle is the capability to read a session).
- **gemini-web cookie paste**: credential field becomes a textarea when the
  preset says `multilineKey`, and `ParseKey` accepts every realistic paste —
  `Cookie: a=b; c=d` header, `document.cookie`, or one cookie per line — keeping
  only `__Secure-1PSID`/`__Secure-1PSIDTS` by EXACT name. The old line-wise
  split stored the whole rest of the header as the PSID value, so a whole-jar
  paste reached Google as a corrupt session; `__Secure-1PSIDCC` (prefix
  sibling) can no longer satisfy the required PSID.
- **Guards**: 4 new `ParseKey` paste-shape tests, `TestGitHubDeviceSlowDownIsSurfaced`,
  `TestDeviceErrClassification` — each bite-proven RED without its fix, with
  the pre-bite hash re-verified after restore; `make test-short` 16 pkgs green,
  gofmt/vet/build clean, `web/dist` rebuilt in the same commit.
- **Combos account select grouped by provider**: hops can already pin an account
  (`combo_hops.account_id`) and `accounts` allows several rows per provider
  (`name`/`namespace` are the only UNIQUE columns — `kind` is not), but the
  dropdown was a flat list of names with no way to see which accounts belong to
  the same provider. It now renders `<optgroup>` per provider, derived from
  `base_url` (no `provider` column — a schema bump is deliberately deferred),
  labelled `groq.com · 2 accounts` only when a provider has more than one; single
  accounts stay bare and the control stays enabled because it also selects the
  provider. Verified against the live account list (16 accounts → 16 clean groups)
  plus the multi-account case (`groq.com · 2 accounts` → "Groq | Groq alt").

## 2026-10-16 — M14 claude-web provider: claude.ai web protocol ✅
Claude.ai's web app (sessionKey cookie + SSE completion) as a provider,
ported from omniroute's production implementation (design:
`docs/claude-web-design.md`, runbook: `docs/claude-web-runbook.md`).
- `internal/claudeweb/tlsclient.go` — uTLS Chrome-146 impersonation
  (`HelloChrome_Auto`, h2 dialer) behind an injectable Fetcher (tests run
  plain HTTP; one test runs the REAL profile against a local h2 TLS server).
  New dependency: refraction-networking/utls v1.8.2 — Go's native TLS
  fingerprint is rejected by claude.ai's Cloudflare.
- `internal/claudeweb/client.go` — sessionKey normalization (CR/LF
  injection rejected), org discovery via GET /api/organizations (the
  zero-quota key-test oracle), the conversation state machine ported from
  the compiled reference: the conversation UUID is CLIENT-generated and
  rides the URL path; follow-ups chain conversation_id +
  parent_message_uuid in turn_message_uuids with a fresh assistant uuid;
  state commits ONLY after message_stop (a cut stream commits nothing).
  Payload parity: opus-5 → high/auto effort, effort vocab
  low|medium|high|xhigh|max, the Default personalized_styles block verbatim,
  create_conversation_params on the first turn, Anthropic-native tool shape
  {name, description, input_schema}. Typed failures: ErrSessionExpired
  (401), ErrCloudflareChallenge (403 + markers), ErrForbidden,
  ErrRateLimited, ErrUnavailable; upstream error events surface; a 401
  mid-stream classifies before the generic stream-cut error.
- `internal/provider/claudeweb.go` + `internal/server/claudeweb_bridge.go`
  — kind `claude-web`, sentinel base `internal://claudeweb` rewritten to the
  in-process bridge (the web SSE is not an ezllm wire), account-id +
  credential stamped in loopback headers and stripped, bridge routes
  /chat/completions (stream + non-stream, history flattening, typed
  refusal for response_format, tools passed through natively) and /models
  (org-discovery auth oracle + the bootstrap catalog). NO model discovery
  exists on this surface: ids pass through, the upstream answer is
  authoritative; the curated set only seeds routing.
- Tests: 11 client tests (cookie forms, the five failure modes, first-turn
  payload parity, follow-up chaining, no-commit-on-cut, error events,
  401-mid-stream, header set, real TLS handshake) + 9 bridge tests + 3
  adapter tests. Bite-proofs: core commit-on-cut, parent chaining,
  challenge detection; bridge ErrForbidden→403 mapping, response_format
  refusal, the removed model gate. Full suite green, -race green.

## 2026-10-16 — M13 github-copilot provider: official CLI device flow ✅
GitHub Copilot as a provider, ported from omniroute's production
implementation (design: `docs/github-copilot-design.md`, evidence:
`docs/github-copilot-policy-evidence.md`, runbook:
`docs/github-copilot-runbook.md`). The auth path is the OFFICIAL CLI's:
device flow with GitHub's public Copilot CLI client id and scope
`read:user` — not a custom third-party app (restricted tier/403), no
client-id spoofing. Live-proved 2026-10-16: the device-code request
against real GitHub succeeds.
- `internal/githubcopilot/auth.go` — device-code request + poll
  (slow_down honored, ctx-cancellable, injectable sleep so tests never
  sleep), `token <gh>` mint at copilot_internal/v2/token (NOT Bearer),
  refresh-token grant with the public client id and NO client_secret
  (9router#442), session encode/decode (JSON row; legacy bare-token rows
  lifted).
- `internal/githubcopilot/provider.go` — endpoint routing (claude* →
  /v1/messages native shim, *codex*/responses → /responses, gemini never
  /responses), full CLI header set with configurable CLI version (the 466
  floor), body quirks (tools ≤128, gpt-5.4 temperature strip, assistant
  reasoning strip, prefill drop, chat-only part sanitize, claude-native
  untouched), /models discovery (chat/enabled/picker filters) with the
  37-model curated fallback, NeedsRefresh 5-min margin, EnsureSession
  mint→refresh→mint chain.
- `internal/provider/githubcopilot.go` — KindGitHubCopilot adapter: session
  decode + per-account in-memory mint cache, /messages → /v1/messages
  anchoring, header set installed over client-supplied values, body quirks
  applied in place, GET /models auth oracle (mint happens in
  PrepareRequest, so calibration exercises the real path), ListModels via
  the minted token, ReadQuota honestly unsupported.
- `cmd/ezllm github-login` — interactive device flow: prints the
  verification URL + code, polls, verifies the mint, lists the account
  catalog, prints the session JSON to store.
- 19 tests (13 protocol + 6 adapter), three bite-proofs red-then-restored,
  `-race` green.

## 2026-10-16 — M12 gemini-web provider: cookie auth, oracle, streaming, bridge ✅
The full Gemini **web** protocol as a router provider, ported slot-for-slot
from the reverse-engineered reference (digest: `~/workspace/gemini-web-api-protocol-digest.md`,
design: `docs/gemini-web-design.md`, runbook: `docs/gemini-web-runbook.md`).
Unofficial surface; risk posture accepted by Calypso: cookie material = live
account access, test accounts, no fallback, typed refusals.
- `internal/geminiweb/` — protocol package:
  - `frame.go` — wrb.fr incremental parser (length-prefixed UTF-16-unit
    frames, boundaries at any byte incl. mid-rune). Its own tests caught a
    units-rescan desync across Feeds (fixed with a `scanned` offset) and
    pinned two fixture traps (raw-string `\n`, wrong unit counts).
  - `client.go` — cookie-pair credentials (PSID required, PSIDTS optional),
    `/app` scrape of the five session tokens, `RotateCookies` (PSIDTS rotates
    ~10 min); expired cookies = the distinct `ErrNoAccessToken`.
  - `rpc.go` — batchexecute + `GET_USER_STATUS`: zero-quota auth verdict
    (AccountStatus 1000/1016/…/1060) AND model catalog from one call.
    Catalog ported index-for-index (capacity tiers Plus 4/12 > Pro > Free
    1/12, model-header build incl. the `gemini-gemini-flash` alias quirk).
  - `generate.go` — `StreamGenerate` request construction (81-slot
    inner_req_list, model-header per-turn mutation) + incremental response
    parse (error code → typed `ErrUpstream` 1013/1037/1050/1052/1060,
    metadata [cid,rid], candidates text@[1][0] with artifact cleanup,
    thoughts@[37][0][0], completion@[8][0]==2, final chunk@[25]) and
    appended-tail delta emission with an incremental UTF-8 decoder
    (`utf8.FullRune` tail test). Tests caught a real double-encode bug
    (`json.RawMessage` inlining where the wire wants a JSON *string*) and a
    final-chunk-before-candidates ordering bug.
- Wiring: kind `gemini-web` + adapter (sentinel base `internal://geminiweb`
  rewritten to the bridge, account+key stamping so bad-key calibration
  exercises the real path, Authorization dropped) + in-process OpenAI-wire
  bridge (`POST /internal/geminiweb/v1/chat/completions`, `GET /models` as
  the auth oracle) + per-account client cache + rotator (8-min cadence)
  persisting via the new `store.UpdateKeyPlain` + typed refusals above the
  package (tools/response_format) + upstream-status→HTTP mapping.
- All mock-proven (29 protocol tests + 4 adapter + 7 bridge-through-sqlite +
  1 store), bite-proven, gates green, `-race` green on touched packages.
  Live cookie proof is operator-gated (runbook).

## 2026-10-15 — M11 gap closure: 1 MiB percentiles, fuzz moat, cancel-latency certificate
- **1 MiB latency percentiles** (`TestLatencyPercentiles`, n=2000, seq):
  stream total p50 9.14 ms / p95 15.65 / p99 16.09, TTFT ≈ total (the gigabyte
  body's parse dominates first byte); non-stream p50 9.77 / p95 16.74 /
  p99 40.15 (one 596 ms max — a swap/GC spike on the loaded box, reported as
  measured). These are e2e totals incl. the mock round trip.
- **Router-only percentile test** (`TestInspectRoutePercentiles1MiB`, n=2000)
  grades spec §18's "inspection+routing, ≤1 MiB, p95 ≤ 5 ms" ISOLATED from
  e2e noise: p95 4.75–5.28 ms across 3 runs — **straddling the target inside
  the noise band** (direct 4.75–5.07, combo 5.07–5.28). Honest grade: NOT
  certified; single-parse's map decode is the cause (old struct-decode ~1.7 ms
  mean, new map-decode ~4.0–4.5 ms mean). The trade stands (e2e −25%) but the
  row is recorded as boundary-UNCERTAIN, exactly as instructed by the spec
  itself ("revise targets based on measurement").
- **Fuzz moat** for the single-parse hot path: 4 targets, seed corpus +
  60 s live fuzz each (FuzzSwapParity: 467 306 execs, 0 divergence). Two
  findings worth their keep: (1) legacy `SwapModel` standing alone TOLERATES
  trailing garbage (dec.Decode stops after the first value) while
  `extractModel` rejected it — the old pipeline rejected such bodies at the
  gate, the new pipeline rejects at `jsonbody.Open`, so PIPELINE parity is
  what the fuzz asserts (oracle = a reimplementation of the old extractModel
  gate); (2) exponent literals overflowing float64 (`1e700`) survive the
  round trip via json.Number — the crasher is now a permanent seed.
- **Cancel propagation certified** (`TestCancelPropagationLatency`, n=1000,
  hard assertion in the test): client-cancel → upstream-cancel **p50 22 µs /
  p95 63 µs / p99 119 µs / max 216 µs** vs the spec's ≤ 20 ms. The §18 row
  now has a measurement instead of a 5 s correctness bound.
- Verified: gofmt/vet/build/full `go test ./...`/-race proxy+jsonbody all
  rc=0.

## 2026-10-15 — single-parse dispatch (M10, perf)
- `perf(proxy,server,compress)` the request body was JSON-decoded up to **5×**
  per request (`extractModel`, `IsStream` per attempt, `SwapModel` per hop,
  `compress.Apply`, `captureShape`). Now: **one decode** — new
  `internal/jsonbody.BodyDoc` (map + `UseNumber`, trailing-garbage rejection),
  opened once in `inference()` and threaded through compression, shape capture
  and every native hop's model swap (`ForwardCandidatesDoc`,
  `prepareHopBody(body, rt, doc)`). Translation legs, multipart STT and admin
  combo-test keep the byte path — `doc` is an optional hint with a legacy
  fallback (no flag; byte-equality tests are the rollback proof).
- Measured (same box as M1 baseline): e2e **−25.0%** non-stream / **−26.1%**
  stream at 1 MiB seq, −10…−16% par4/par16, −13.7% stream par16 at 64 KiB;
  e2e allocs ~748→~690/op. Honest negative: `InspectAndRoute` in ISOLATION
  got heavier (the one shared parse is a full map decode where the old
  single-field struct decode skipped the messages array) — the request-level
  metric is the one that counts. All numbers + the two bite proofs in
  `docs/single-parse-dispatch-design.md`.
- `prefix_sha` now sees JSON number literals verbatim (`1e10` and
  `10000000000` hash differently, big ids keep exact digits) instead of
  collapsing through float64 — correct for a cache-BREAK detector; documented.
- Verified: full `go test ./...` green with zero edits to existing call
  shapes (doc=nil path exercised by the entire old suite), byte-equality
  tests vs legacy `SwapModel` on 7 fixture classes (unknown fields, big ints,
  unicode, nested tools, dup keys), stream-flag parity, captureShape parity,
  two bite proofs (UseNumber drop → mangled big int; model-assignment drop →
  upstream sees `"model":null`), gofmt/vet/build/race green.

## 2026-10-13 — chat searchable model picker + per-model Test in the providers drawer
- `feat(web)` chat model `<select>` → **searchable combobox**: typing filters by model id
  OR label (namespace prefix included), ↑/↓ highlight, Enter picks, Escape/click-outside
  closes; empty states (loading / no models / no match) preserved, same input classes so
  the controls row does not move. Built for the "too many models to scroll" complaint.
- `feat(web)` providers models drawer gets a **`test` column**: a per-row `Test` button
  probes JUST that model (the sync endpoint's `models` scope, force re-test) instead of a
  whole `Test all models` sweep. Curated rows disable the button with an explanation —
  probe writes cannot overwrite an operator-curated verdict (server-proven), so clicking
  would only spend. While a sweep runs, the row button spins and the header line carries
  the clock; a scoped probe of an id that left the live catalog reports THAT as the cause.
- `api.ts` `startSweep` accepts `models?: string[]` (scoping the sweep; the catalog
  refresh still upserts the FULL list — a one-model test never prunes the drawer).
- Verified: `tsc`+vite green; live browser walk at 390×844 — all 8 routes
  `scrollWidth == clientWidth == 390`, `scrollX` stays 0, models drawer shows the 4-column
  table with 80 Test buttons and no overflow; combobox filter/enter/arrow/escape all
  exercised in the live page.

## 2026-10-13 — Cloudflare Workers AI key test fixed (account-aware oracle + native catalog)
- `fix(provider,registration)` adding Cloudflare Workers AI failed with `cannot verify this
  key: no credential-checking endpoint and no model to probe with / This gateway exposes
  neither /usage nor a catalog`. Two separate misses, both live-measured 2026-10-13:
  - **Oracle** — the generic `GET {base}/usage` does not exist there (405), and
    Cloudflare's real check lives outside the account base URL:
    `GET https://api.cloudflare.com/client/v4/user/tokens/verify` → 200 `{"status":"active"}`
    for a valid token, **401** for a well-formed `cfut_`+40-hex fake, but **400 format
    error** for a malformed fake — which calibration would have read as inconclusive.
    New optional `HostedAuthOracle` interface gives the adapter the ACCOUNT so
    openai-compatible can return a host-scoped oracle; `AuthProbe.CalibrationKey` carries
    the format-valid fake; `doProbe` now accepts an absolute `http(s)` Path (used verbatim,
    everything else still joins at `UpstreamURL`).
  - **Catalog** — the OpenAI-compat prefix has NO `/models` (405 live), and the native
    `GET /client/v4/accounts/{id}/ai/models/search` answers the Cloudflare envelope
    `{"success":true,"result":[...]}` which the OpenAI parser reads as ZERO models.
    `isCloudflareAI` (kind+https+host+`/ai/v1`, OllamaCloud parity) switches `ListModels`
    to the native envelope, skipping id-less entries.
- Non-Cloudflare openai-compatible accounts are untouched (explicit regression test), and a
  `success:false` catalog envelope now surfaces the provider's own words instead of
  passing as an empty catalog.
- Verification: 5 new tests (e2e key test through the real flow + detector + oracle shape +
  envelope parse + non-leak), **bites 3/3 fell for the right reason** (hosted oracle
  removed / CalibrationKey ignored / catalog branch removed), full suite green.

## 2026-10-13 — NVIDIA NIM key-test fix: measured-fast candidates, honest timeout verdict
- `fix(registration)` adding NVIDIA NIM failed with `generation probe failed: Post
  "https://integrate.api.nvidia.com/v1/chat/completions": context deadline exceeded`, and
  the hint blamed a provider rejection for a response no provider ever sent. NIM's shared
  free tier answered `deepseek-ai/deepseek-v4.1-flash` (the generic buckets' FIRST pick, by
  name) with nothing inside 90s and `z-ai/glm-5.3-flash` with 43.7s, against
  `defaultStepTimeout = 15s`. Ledger evidence for the failed add: `admin:test …
  deepseek-ai/deepseek-v4.1-flash status=0 15000ms`.
- `pickProbeModels` now leads with `nvidiaNIMProbeModels` on this host (host-scoped like
  `isOllamaCloud`, preset base URL `https://integrate.api.nvidia.com/v1`):
  `nemotron-3.5-lightning-30b-a3b` 0.6s · `llama-3.2-11b-vision-instruct` 1.7s ·
  `gpt-oss-20b` 6.2s — measured, matching the probe playbook's "pick by measurement, never
  by 'flash'/'mini' in the id". Each entry is matched against the account's own catalog; a
  full miss falls through to the generic buckets (NIM rotates ids: `meta/llama-3.1-8b-instruct`
  went EOL 2026-08-26 while still listed).
- A transport error/timeout in the generation probe is no longer `probeFailed` with a
  rejection hint: it carries an honest "did not answer within 15s" verdict and ADVANCES to
  the next candidate; only the last candidate's timeout fails the test. 401/403 and 4xx
  verdicts are unchanged.
- NIM's `/v1/models` verified PUBLIC (200 + full catalog for a garbage key), so the catalog
  can never prove a credential — the generation probe stays the gate.
- Verification: 5 new tests (2/2 bites fell for the right reason), full suite green, and a
  live scratch-instance proof through the real handler — `tests/prove_nim_keytest.py`,
  201 in 2.1s, `inference: nvidia/nemotron-3.5-lightning-30b-a3b · 766ms`, ledger row
  `status=200`.

## 2026-10-13 — provider presets: NVIDIA NIM, Cloudflare Workers AI, OpenRouter
- Dashboard add-provider presets: **NVIDIA NIM** (`https://integrate.api.nvidia.com/v1`),
  **OpenRouter** (`https://openrouter.ai/api/v1`), and **Cloudflare Workers AI**, whose
  account ID composes the endpoint `https://api.cloudflare.com/client/v4/accounts/{id}/ai/v1`.
  All three are ordinary `openai-compatible` accounts — Workers AI serves the whole OpenAI
  family under that prefix (chat, responses and an anthropic-compatible `/messages`), so no
  new protocol was needed. The `cf-aig-gateway-id` header only applies to Cloudflare's AI
  Gateway (`/ai/run`), not to these endpoints.

## 2026-10-13 — strategy citizenship, the hold checkbox, request IP (M7; local, not pushed)
- `feat(router)` two operator decisions on the universal matrix: translated hops are ordinary
  combo members (the old "native hop always wins" tail rule is removed), and
  `translation.universal_enabled` is the SINGLE switch for the matrix (no longer requires
  `translation.enabled`) (`b1a2f5b`).
- `feat(store,router,web)` schema v14 — `combo_hops.hold_until_failure`: a block_round_robin
  hop the operator checks HOLDS its block while it keeps succeeding and releases it on a
  hop-attributable failure (429/5xx/transport/failed-truncated stream; a 4xx keeps the hold).
  Combo editor checkbox + a `hold` chip in the combo list.
- `refactor(router)` `strict_round_robin` REMOVED (no live combo used it; its success-driven
  advance is now block_round_robin with a held hop).
- `feat(store,server,web)` schema v14 — `calls.ip`: the caller address (X-Real-IP, then the
  first X-Forwarded-For entry, then the socket peer) captured for traffic mapping; shown in
  the dashboard under the request timestamp. Lat/long dropped by operator decision.
- `feat(web)` requests explorer: tokens in/out consolidated into one `tokens ↑↓` column with
  hover titles; the feed's arrows get proper hover titles too.
- Migration rehearsed on a live-DB copy: v11→v14, calls 6052 / accounts 11 / models 624 /
  combos 11 / hops 39 / tokens 6 preserved, zero invented holds or IPs, second boot idempotent,
  production untouched at v11.

## 2026-10-12/13 — universal wire matrix (M0–M6; local, not pushed)
- `fix(translate)` M0: Responses streams lost tool arguments when the item id and call id
  differed, and dropped terminal usage on incomplete/failed runs (`00232c5`).
- `feat(router,proxy,server)` M1: wire identity — `Route.WireSurface`/`EffectiveWire()`, the
  wire decided BEFORE capability/pin gates, typed `wire_unsupported` (400) with zero upstream
  calls for a disproved destination (`14e4730`).
- `feat(translate)` M2: surface-neutral request IR + one codec per surface; unsupported
  features refuse with a reason; absent vs zero preserved (`3a3ab3c`).
- `feat(translate)` M3: shared response/event codecs with the legacy wrapper cutover —
  byte-identical to the pre-cutover package across a 35×3 + 35×3 corpus (`f7ee36b`).
- `feat(store,provider,translate)` M4: `accounts.usage_convention` (schema v13) — the
  operator-declared Messages input-token accounting; a cross-wire Messages destination refuses
  an unstated convention. Also fixed two pre-existing bugs: a partial PATCH blanked `notes`,
  and `UpdateAccount` swallowed non-conflict write errors (`7294755`).
- `feat(proxy,router,server,config)` M5: the three new cells (chat→messages, responses→chat,
  responses→messages) behind `translation.universal_enabled` (default OFF; env rollback), the
  fixed fallback chains, pin-as-intent, and the completion outcome (`Result.StreamOutcome` +
  ledger err; late stream failures withhold the hop-good signal) (`5a145dd`).

## 2026-10-10 — M9: research schema (v8) + honest savings + TTFT + cost
- `fix(proxy)`: TTFT was timed from inside the stream copy — after headers had already
  arrived — so every row read 1–3ms. Now timed from upstream dispatch; the number finally
  means time-to-first-token, which is what caching research measures (`f48d20a`).
- `fix(compress)`: `Saved` was set before the ratio-floor check, so rejected rewrites still
  reported savings (prod week: 18.2k real vs 21.4k notional in one SUM). `Saved` is now 0 on
  any skip; `SavedNotional` carries the rejected potential (`19b743d`).
- `feat(store)` schema v8: request-shape capture (`prefix_sha` of system+tools+history-minus-
  newest-turn, `msg_count`, `tool_count`, `session_id` via `X-Ezllm-Session`, `req_bytes`) —
  the cacheability data `raw_usage` could never reconstruct; `compression_saved_notional`;
  versioned `model_prices` (account×model×date) with cost DERIVED at query time, unpriced
  calls counted not zeroed (`393460b`).
- `feat(server)`: capture runs on the inference path after compression (a history rewrite
  measures as the cache break it is); admin API `GET /admin/prices`,
  `PUT/DELETE /admin/accounts/{id}/prices`, `GET /admin/cost`; requests projection surfaces
  the shape columns (`0e24e32`).
- `feat(web)`: Economics cost panel (spend, priced vs unpriced, per day/account/model) +
  Providers Pricing drawer (per-account rate versions; all four rates required) (`ec27973`).

## 2026-10-09/10 — providers batch, UI cleanup pass, surface pin (deployed 2026-10-10)
- `feat(provider)`: fourth kind `gemini-openai` — Gemini's OpenAI-compat endpoint; its bad-key
  signal is `400`, honored by the auth gate only for that kind via the optional
  `AuthRejectStatuser` (`a8776ee`).
- `feat(web)`: provider presets — Command Code, DeepSeek, TokenRouter, Gemini, Groq built-in
  (`5335423`, `1d3f3d7`, `fb2a2fd`).
- `feat(registration)`: dropped all API-key **shape** checks; the probe chain is the sole
  validator, so exotic key formats are judged by the upstream (`9aa9cf1`).
- `fix(server)`: `GET /healthz` is liveness-only (`{"status":"ok"}` after a DB ping) — the
  public route no longer leaks counts, paths, or versions (`9a7b620`).
- `feat(web)`: dashboard UI cleanup pass — shared `ConfirmModal` replaces `window.confirm`
  and one-click deletes; Providers key status shown as text (`tested ok` / `test failed` /
  `untested`) not a bare dot; quota copy "quota not available"; Connect drops the Responses
  surface from the picker (server route unchanged), 2-column desktop layout; Dashboard strip
  trimmed (no ledger/dropped/schema chips, no context column); combo "custom model id" flag now
  defaults from what's actually rendered, and the profile-number pill is gone (`c099dd5`…`9915582`).
- `feat(pin)`: per-model **surface pin**, enforce-only (schema v7 `models.proto_pin`). A
  wrong-surface call to a pinned model is refused `400 pinned_surface` before any upstream
  dial; never a reroute (bodies pass through untranslated). Lookup fails closed; catalog
  resyncs preserve pins (`340442b`).
- `fix(server)`: the pin refusal reached clients as `502` while the ledger logged `400` —
  the proxy error switch had no `ErrPinned` case. Now `400 pinned_surface` with structured
  `{pinned, requested_surface}`; live-proved on prod (`03a6317`).
- `build(web)`: **rebuilt and committed `web/dist`** — the image embeds it, so source-only UI
  commits shipped the old bundle; after every UI commit, `npm run build` + commit `dist` +
  verify the served hash post-deploy (`831ecda`).
- GitHub Copilot: OAuth app registered, device flow works end-to-end, but the
  `copilot_internal/v2/token` exchange returns `403` for third-party apps → parked, not
  circumvented. OAuth artifact deleted.

## 2026-10-09 — post-ship fixes
- `fix(proxy)`: strip client `Origin` before forwarding — Anthropic reads any browser `Origin`
  as CORS and rejects with a 401; regression-tested (`8c832ef`).
- `fix(web)`: clear the dashboard session only on ezllm's own auth 401; provider 401s surface
  inline (`358cda3`).

## 2026-10-08 — Docker, dashboard polish, budget engine
- Distroless image + home-box Traefik overlay; explicit router priorities and a dedicated
  `/admin/login` limiter (`3f46f00`, `3155b76`).
- `POST /admin/chat` behind the real routing path; Chat page, Token Economics, budget controls
  (`d0f8407`, `d8d0f68`).
- Budget stage with its own context-token accounting (schema v5) (`39cbdb8`).
- Combo retry with exponential backoff before failover; `EZLLM_*` env overrides restored for
  containers; per-combo enable toggle; enabled combos advertised in `/v1/models`.
- Provider add-in-one-dialog (key validated on save) with a Claude Platform preset; lm-eval
  canary proving traffic goes through the gateway.
- MIT license.

## 2026-10-07 — compression, rules, live surface
- Compression: M5 pipeline (engines, profile CRUD, inference wiring); headroom + lite engines
  (M6.5 phase 2); caveman lossy prose condensation, gated by construction (M7).
- Rules eligibility engine — token caps + allowed-use windows; dashboard rules editor.
- Live recent-requests feed (SSE over fetch); stats explorer (granularity × range); Requests
  page with token dissection.
- Dashboard password + plaintext provider keys (explicit operator choice).

## 2026-10-06 — core: M1–M4
- M1: skeleton + health.
- M2: store + field crypto + three-surface passthrough (`/v1/chat/completions`, `/v1/messages`,
  `/v1/responses`), usage normalized into the SQLite ledger.
- M3: admin API + embedded dashboard; registration with capped key-test protocol sweep; live
  model-catalog probes; adapters (`opencode-go`, `openai-compatible`, `anthropic-compatible`).
- M4: combos resolve to ordered candidates + strategies; proxy attempt/commit split (failover
  that never leaks a failed hop); admin persistence (accounts, provider keys, combos, tokens).
