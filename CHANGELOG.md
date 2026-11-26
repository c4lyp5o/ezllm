# Changelog

Notable changes, reverse-chronological. Milestone-tagged (M1–M15), then by release; full history via `git log`.

## Unreleased

- **Every key of a provider is used.** A provider with several keys now takes turns:
  each request starts on the next key. A `429` or a refused key (`401`, `403`) parks only
  that key for that model, and the request moves straight on to the next key. A server
  error is not retried on the other keys. The provider shows **parked** only when every
  key is; until then its card reads "1 of 2 keys parked". `GET /admin/cooldowns` names
  the key, and route explain reports `key_count`.
- **Export to CSV or JSON.** Requests downloads the rows it shows, and Token Economics
  its spend by day, provider and model. Text that could run as a spreadsheet formula is
  neutralised.
- **Quick jump.** Ctrl+K (⌘K on a Mac), or **Jump to…** in the sidebar, opens a palette
  of every page, the Settings tabs and the common "new …" actions. Type to filter,
  Enter to go. `/providers?add=1` opens Add provider.
- **Rule groups save in one step.** Editing, turning off or deleting a rule group is one
  transaction, however many models it holds: every rule changes or none does. It was
  one request per model, so a failure halfway left some rules changed. New `POST
  /admin/model-rules/batch`.
- **Explain a route.** **Explain** on a combo shows what a request would do right now,
  hop by hop: tried, skipped and why (turned off, disabled account, missing model, a
  rule, a park, too little context, no tools), translated or native, and whether a
  chosen key may call it at all. Nothing is sent to a provider. New `POST
  /admin/route-explain`.
- **Rotate the master key, create keys from the command line.** `ezllm
  rotate-master-key` re-encrypts every stored secret under a new key in one step and
  swaps the key file, keeping the old one until you delete it; any failure changes
  nothing. `ezllm token create --name NAME` makes a client key and prints it once.
- **Services page loads gently.** Provider catalogs load four at a time instead of
  all at once; models still appear as each provider answers.
- **Unpark now.** Parked providers show **parked** with a live countdown on their
  Providers card and on the Combos hop. **Unpark** sends them traffic again at once,
  after you fix the cause. New `GET|DELETE /admin/cooldowns`.
- **Compression per key.** A key can carry its own compression profile (Connect → key
  policy). It applies to that key's requests on every route, direct `namespace/model`
  calls included; a request's `x-ezllm-compression` header still wins, and a combo's
  profile applies when the key has none.
- **One model by id.** `GET /v1/models/{model}` returns that model's entry, so SDKs
  and IDE plugins that check a configured model at startup no longer get a 404.
  Anthropic clients get Anthropic's shape.
- **Release check.** ezllm asks GitHub once a day for its latest release and says
  when a newer one is out: the sidebar version becomes a pill, Settings → System
  names the release, and the bell rings once per version. On by default;
  `update_check` in Settings or `EZLLM_UPDATE_CHECK=off` turns it off. New
  `GET /admin/update`; `/healthz` is unchanged.
- **Bridge updates one CLI at a time.** The updater swaps, restarts, proves and, if
  needed, restores each bridge on its own. A failure in one no longer undoes the
  others. Expect one restart per updated bridge instead of one shared restart.
  A temporary failure after the switch is tested once more before it is undone.
- **Antigravity plan limits.** The provider card and Quota dialog show how much of each
  weekly limit is used, per group of models, read with `agy -p /usage` (no quota spent).
- **Dashboard fixes.** Links in chat answers open only when they are `http`, `https`
  or `mailto`. The chat model picker lists combos. Neighbourhood buildings take their
  totals from their own account, so two keys with the same hint no longer double the
  traffic, and "View requests" filters by namespace. Connect shows a capped key's use
  in its current calendar bucket (the number the cap enforces) and counts the spend
  cap; `GET /admin/tokens` now carries `cap_used` and `spent_usd` for capped keys. A
  hand-set model verdict has a Release button. The Console cookie field appears for
  Claude Platform, and credential fields follow the base URL. Savings read only
  rewrites that shipped. Opening a link in a new tab no longer asks for the password
  again, and the live feed stops retrying after the session ends. Other fixes: allowlist
  globs match the server, feed rows no longer drop same-second calls, token tooltips say
  whether cached tokens are inside, Stats links through by client and combo, every
  notification opens its page, the model picker closes when you tab away, binary chat
  attachments are refused, the ratio floor goes down to 0%, and undefined colours are
  fixed.
- **Compression, backup and logging fixes.** A caveman rule at standard intensity
  wrote a literal `$1` into prompts in place of punctuation ("Hello.  World" became
  "Hello$1 World"); the rule is removed, and the pack loader now refuses any
  replacement with a capture reference. A message that mentions ``` mid-sentence no
  longer turns compression off for the whole conversation: the fence check now asks
  whether a rewrite unbalanced a message, across tool results and Responses items
  too. The session memo keeps a conversation's opening messages when its cap is
  reached. The rule-length gate measures the true shortest match. Progress bars
  redrawn with an erase-line no longer leave the old bar's tail. Backups no longer
  fail on filesystems that cannot flush a directory. Log messages escape newlines and
  terminal escapes. Combo `sticky_idle_s: 0` ("never release on idle") can be saved
  with PATCH. The Codex bridge logs readable lines, and a template folder it cannot
  remove no longer costs a template slot.
- **Faster compression and uploads.** Caveman's span protection skips patterns whose
  characters are absent: 4.3 times faster on plain prose (10.6 to 2.45 ms on 18 KB).
  Sizing the conversation between stages no longer builds a JSON copy each time, and
  the session memo hashes each message once instead of three times: a four-stage run
  on a 360 KB conversation is 14% faster and allocates 27% less. Swapping the model in
  a 25 MiB multipart upload uses 32 MB instead of 87 MB. The database refreshes its
  query statistics when ezllm stops. Caveman skips a rule when the words it needs are
  absent: with the other changes, that four-stage run went from 642 to 205 ms.
- **Audio and video stream through.** Speech, video and other binary answers reach
  the client as they arrive instead of after the whole file is generated and held in
  memory. A file cut off mid-way is recorded as truncated.
- **Codex and Copilot bridge fixes.** The Copilot bridge starts its CLI in its own
  process group and ends the whole group, so nothing the CLI started outlives a
  cancelled turn or a stopped worker. Codex's `--help` check matches whole flags, so
  `--sandbox` is no longer found inside `--sandbox-policy`. Copilot error details are
  cut on a character boundary.
- **Claude and Antigravity bridge fixes.** A Claude Code CLI that stops without an
  answer is now a 502 with its own words, not a 200 with an empty message. Ending a CLI
  no longer looks up its process group after the CLI is gone, so a recycled process id
  cannot be killed by mistake. A Claude conversation keeps its session id when the client
  moves its cache marker, so the cached prefix survives turn 2. The Claude and
  Antigravity bridges log readable lines again. The bridges accept `bearer` in any case
  and refuse a token sent without the scheme. Antigravity plan-usage reads run one at a
  time and are reused for 30 seconds, instead of starting a CLI per refresh. An
  Antigravity answer whose deltas stop short streams the rest. Output too large to read
  is reported as such. The `--help` flag check tolerates small format changes.
  A non-streaming Claude reply names the stop sequence that ended it.
- **Provider fixes.** claude-web answers that stream longer than 60 seconds are no
  longer cut off: the timeout now covers only the wait for the first response. A
  claude-web answer that is a tool call fails with `chat_tools_unsupported`, so a combo
  moves on, instead of ending as an empty answer. claude-web reads `deflate` answers,
  releases its zstd decoders, uses the organization the browser marks active, and sends
  `UTC` when the host has no named timezone. Probing an OpenAI reasoning model no longer
  marks it as not speaking chat because it refused `max_tokens`. Gemini's per-minute limit
  reads as `rate_limited`, not `quota_exhausted`. "reasoning_effort is not allowed" is no
  longer learned as "reasoning required". A query string reaches the claude-web and
  gemini-web bridges intact. A key test sends an account's custom headers exactly as
  live requests do. gemini-web's request counter no longer goes negative after about
  21,000 requests.
- **Routing and rules fixes.** At start-up, one rule with a timezone that does not
  resolve no longer leaves every rule off: the valid ones load and the bad one is
  logged. When every combo hop is skipped and one was refused by a rule, the client
  gets 429 with when to retry, not 502. `next_allowed` skips days a window does not
  allow. Accounts without a rate limit no longer take a global lock on every request.
- **Translation fixes.** A field sent as `null` is treated as left out: SDKs send unset
  options that way, and `max_tokens`, `temperature` and `top_p` became 0, parallel tool
  calls turned off, and `tool_choice: null` failed the request. A failed tool result
  (`is_error`) keeps its failure, as `[tool error]`, instead of reading as empty success.
  Images reach `responses_models` hops as `input_image` inside a user message. Text after
  a tool call in a chat stream no longer aborts the answer. A tool-call-only assistant
  turn sends `content: null`. Image data URIs without `;base64` are read correctly.
- **Proxy fixes.** Client-address and browser headers (`X-Forwarded-For`, `X-Real-IP`,
  `Forwarded`, `Cf-Connecting-IP`, `Referer`, `Sec-Fetch-*`) no longer reach providers.
  An upstream error sent as a stream on a translated hop reaches the client as its own
  error, not run through the stream translator, and an error after a broken stream
  starts on its own line. `408`/`425` use the server-error cooldown. Gemini audio over
  32 MiB is refused instead of cut. Model IDs are escaped in Cloudflare and Gemini URLs,
  and Cloudflare is recognised by host. A later zero no longer wipes an output token
  count. When every hop is too small for the request, the error says so instead of
  "five-minute cooldown".
- **Store fixes.** Turning an account off in the dashboard (or any partial account
  `PATCH`) used to wipe its custom headers, token cap, context size, quota mode and cap
  window; it keeps them now, and a `PATCH` is validated like a new account. A rule's
  `+08:00` timezone works; it used to drop the rule or count in UTC. Changing a rule's
  model moves the rule. Passwords over 72 bytes get a clear 400 instead of a 500. A new
  account never reuses a deleted one's id, and deleting an account clears its usage
  counters. Negative upstream token counts read as 0. A management key that cannot be
  decrypted is dropped instead of sent upstream. The web providers drop `developer`
  messages like `system` ones and refuse images instead of ignoring them.
- **Admin API fixes.** A rule `PATCH` changes only the fields sent; `{"enabled":false}`
  used to zero its cap and delete its allowed hours. Renaming a combo renames it instead
  of creating a second one. A price import with one bad row writes nothing.
  `count_tokens` now counts against the request limits. A sync body sent without a
  length is read, and an unknown surface name answers 400.
- **Start-up checks.** `=false`, `=0` or `=OFF` on `EZLLM_PRICE_REFRESH`,
  `EZLLM_UPDATE_CHECK` or `EZLLM_COMBO_AUTOPROBE` used to turn the feature on; every
  on/off switch now takes on/off, true/false or 1/0 in any case, and anything else
  stops the start. Provider requests honour `HTTPS_PROXY`/`NO_PROXY` and time out
  connecting and the TLS handshake. A bridge's token now defaults to the data folder,
  is written whole, and a bridge port already in use stops the start. The Claude
  bridge stops the start when its CLI does not run or `USER`/`LOGNAME` are missing,
  and its checks time out. Unknown `config.yaml` keys are logged as warnings, and a
  bad `listen` port is refused.
- **Your prices stay yours.** The daily price refresh skips a model you priced by hand
  or by import. It used to write the snapshot's rate over yours. The refresh answer
  counts those models in `kept`.
- **Docs, latest version only.** User docs now describe only the current version, in
  the task style, and say less: metrics, backups, settings, master key, prices,
  logging, environment, translation and the web providers were rewritten.
- **Bridges on Windows.** A guide for running ezllm in WSL (docs/wsl.md). The macOS script
  now makes its data folder private, and the macOS update steps fetch the script again.
- **Claude on macOS.** The bridge reads the login that Claude Code keeps in the Keychain.
  It used to answer "Not logged in".
- **Antigravity reasoning level.** The model sets it (`gemini-4-hydrogen-high`). The
  bridge ignores a client's `reasoning_effort`; a level that differed from the model's
  used to fail the turn.
- **Binary install guide.** The README installs the Linux and macOS binary in one paste,
  and `docs/install-binary.md` is steps first, for Linux, macOS and Windows. On Windows,
  `Stop-ScheduledTask` left ezllm running and the task stopped after three days; both are
  fixed in the steps.
- **ez standard CI.** `publish.yml` replaces `docker-publish.yml` and `release.yml`:
  pushes to `development` publish `:development` and `:sha-<commit>`, `master`
  publishes `:latest`, `:X.Y.Z` and `:sha-<commit>`, and a tag publishes `:X.Y.Z` and
  `:vX.Y.Z` with the release binaries and this file's section as the notes. CI also
  builds and boots the image. `CONTRIBUTING.md` is now `DEVELOPER.md`.
- **Copilot CLI 1.0.113 and newer.** The Copilot bridge speaks headless protocol 4
  as well as 3: it sends `session.create` in the shape the connected CLI documents
  and still refuses any other protocol. Proven live on 1.0.112 and 1.0.114. The
  session-cache gate uses `gemini-4-hydrogen` unless `--model` names another.
- **Antigravity caller tools on by default.** The structured-output adapter
  turns on at boot whenever the CLI offers `--json-schema` (`cmd/ezllm/agybridge.go`);
  a CLI without it boots text-only with a warning. `EZLLM_AGY_EXPERIMENTAL_TOOLS`
  is retired and fails the boot. The conversation cache follows, on wherever the
  CLI also supports `--conversation`. `bridge-check agy` now fails a CLI without
  `--json-schema` and always runs the caller-tool live proof.

- **Bridge setup guide.** `docs/bridges/` has one page per plan, for Docker Compose,
  `docker run` and the Linux binary, and one troubleshooting page with a checker; `docs/bridge-setup.md`
  is the index. The Claude and Codex guides staged the CLI where ezllm does not look
  (`claude-VERSION`, `/opt/codex`). A bridge whose CLI is missing names the guide when it
  stops the boot.
- **Docs for one version.** User docs describe the version they ship with; what to
  change when coming from an older one is in `docs/upgrading.md`. The bridge runbooks are
  reference only, without setup steps. The dashboard's bridge links open the docs of the
  running version (release tag, or `development`); `GET /admin/bridges` gains `setup` and
  `docs`, and its 409 names the setup page.
- **Design records moved** to `docs/design/` (with the two bridge specs from the root), so
  `docs/` holds only what someone running ezllm needs.
- **macOS installer.** `scripts/ezllm-mac.sh install` gets the latest release instead of
  1.3.2; `EZLLM_VERSION` still chooses one.
- **Neutral names.** Docs, examples, test fixtures and comments no longer name the
  maintainers' own accounts, hosts, clients or paths. `scripts/ezllm-mac.sh` defaults
  `EZLLM_USAGE_TZ` to UTC, and the live proof scripts default to `http://127.0.0.1:20129`,
  with token files under `~/.config/ezllm/`. The exported request records and the
  security audit reports are no longer in the repo.

## v2.2.0 — shared bridge admission, requests per turn, token counting, spend caps

- **`POST /v1/messages/count_tokens`** answers Claude Code's token-count
  calls instead of 404. `proxy/count_tokens.go` `CountTokens` asks up to three
  candidate hops on the Anthropic wire (skipping parked ones) and returns the
  first `input_tokens`; a miss never cools a hop down. Otherwise
  `server/count_tokens.go` estimates over system, messages and tools at 2
  characters per token, above the ~2.35 Anthropic measured, so it leans high. `X-Ezllm-Token-Count` and
  `ezllm_count_tokens_total{source}` show how often each path answers.
- **Per-key spend cap (schema v38):** `client_tokens.cap_usd`, enforced in
  `authenticate` by `server/spend_cap.go` `admitSpend`. Spend is
  `store.ClientSpendStatus`, which prices the key's calls in the current
  window the way the cost report does (calls grouped per account, model and
  day before the price lookup). The answer is cached for 15 seconds per key, and a
  `PATCH` drops the cache so a new cap applies at once. The dashboard's key
  policy dialog has the field.

- **Review fixes:** Claude streaming commits headers only with its first visible
  event, preserving HTTP 429 and `Retry-After` on queue refusal and recording
  slot wait. Turn keys include the conversation prefix, surface, optional session
  and stateful Responses parent so unrelated histories do not merge in Stats.
  `cache_control` markers are left out of the key, so a moving cache breakpoint
  no longer splits one Claude Code turn in two.

- The Copilot bridge's registry id is `copilot-sub` (was `github-copilot`, the
  retired provider's name), matching its preset key like the other bridges.
  `TestBridgeIDsMatchTheirPresets` pins all four ids.
- **Shared bridge helpers:** process-group start/kill (`SetProcessGroup`,
  `KillGroup`) and the OpenAI error envelope (`OpenAIErrorEnvelope`) moved
  into `internal/bridgeutil`, replacing identical copies in the Claude, Codex
  and Antigravity bridges. No wire or behaviour change; `TestKillGroupReachesGrandchild`
  and `TestOpenAIErrorEnvelopeShape` pin them.
- **Shared chunk encoder:** Codex, Copilot and Antigravity write their
  OpenAI chunk streams through `bridgeutil.ChatChunks`; event bytes are
  unchanged (`TestChatChunksWireBytes`). Copilot streams now send
  `X-Accel-Buffering: no`, so a reverse proxy no longer buffers them, and drop
  the redundant `Connection: keep-alive`.
- **Shared admission (behaviour change):** all four bridges admit through
  `bridgeutil.Slots`. Claude, Codex and Antigravity now bound their wait
  queue like Copilot: past `EZLLM_BRIDGE_QUEUE` waiting requests
  (default 8) they answer 429 `queue_full` with `Retry-After: 5`
  instead of holding the request until the 300 s timeout; the router parks the
  hop briefly and fails over. Every `/status` reports the same `runtime`
  admission counters.
- **One queue setting (breaking for a renamed variable):** `EZLLM_BRIDGE_QUEUE`
  sizes every bridge's wait queue. `EZLLM_COPILOT_BRIDGE_QUEUE` is retired and
  fails the boot, naming its replacement; rename it in compose before upgrading.
- **Shared auth (behaviour change):** one constant-time token check
  (`bridgeutil.Authorized`). Codex's 401 type is now `invalid_request_error`
  like the other OpenAI-shaped bridges; Claude keeps `authentication_error`
  and `x-api-key`.
- **Bridge slot wait in the ledger (schema v37):** `calls.slot_wait_ms`
  records how long a bridge request waited for a CLI slot, which `total_ms`
  used to hide. Bridges send it in `X-Ezllm-Slot-Wait-Ms`; the proxy trusts it
  only from loopback and strips it from client responses. Older rows stay
  NULL. The dashboard shows it as "queued" under the total and in the
  request detail.
- **Bridge load on the dashboard:** `GET /admin/bridges` carries each running
  bridge's live `runtime`, and Settings → System shows running / waiting /
  served / refused per bridge (diagnostics carries only those counters).
- **Queue-full warning:** a bridge that refuses with `queue_full` logs one
  `WARN` per 10 s naming the bridge, its limits and the refusals since the
  last line.
- **Requests per turn (schema v37):** every ledger row carries `turn_id`,
  the agent loop it belongs to (one human message plus the tool calls after
  it), derived from the client's body so clients need no session header.
  `GET /admin/turns` and a Stats panel show requests per turn by model, so a
  model that stops early shows up in the data instead of needing a replay.
  An Anthropic `user` message carrying any `tool_result` block is an agent
  step even with text beside it (Claude Code's system reminders), a stateful
  Responses step (`previous_response_id`, tool outputs only) joins the turn of
  the response it continues, and failed requests (status 400 and up) are not
  counted.
- **gemini-web survives cookie rotation:** a rotated credential is two lines
  (`__Secure-1PSID`, `__Secure-1PSIDTS`), and the proxy stamped it raw into
  `X-Ezllm-Gemini-Web-Key`, which Go rejects for the newline. Every request
  after the first rotation (the first request rotates) failed, which looked
  like a session expiring in minutes. The header is now base64; the bridge
  decodes it (`TestGeminiWebStreamAfterCookieRotation`).
- **Copilot model catalog off the request path:** the live `models.list`
  catalog refreshes in the background (every 10 minutes, 1 minute after a
  failure) and is warmed at boot. Requests read the last good list and never
  wait for a refresh, so a slow or hung CLI no longer stalls the bridge.
- **Cleanup:** the bridge token file, HTTP startup, stderr tail buffer, raw
  JSON writer and id helper are one shared copy each (`cmd/ezllm`
  `bridgeTokenFromFile`/`serveBridge`, `bridgeutil` `util.go`); unused code
  went (`withPooledChannel`, `landlockSupported`) and three test-only wrappers
  were removed with their tests pointed at the production entry points. No
  behaviour change; token files keep their paths.

## v2.1.0 — Prometheus metrics and bridge updater fixes

- **Metrics:** `GET /metrics` serves Prometheus text exposition, hand-written
  with no new dependency. `store.ObserveLedger` hands each committed ledger
  batch to the collector on the ledger goroutine, so the request path is
  untouched and counters match the ledger; `BenchmarkMetricsObserve` measures
  about 255 ns per row with zero allocations. Labels are bounded to surface,
  provider account, upstream model and status, and each family stops at 2,000
  series (`model="_other"` beyond). Read only with a `metrics`-role key. Design: `docs/metrics.md`.
- **Roles:** client keys take any non-empty set of `infer`, `metrics` and
  `admin` (`store.NormalizeRoles`). Inference routes, including `/v1/messages`
  through `x-api-key` and the VS Code alias, now require `infer`
  (`Server.inferAuthed`); before, any valid key could call models. Schema v36
  grants `infer` to admin-only keys so existing keys keep working. Each role
  opens one surface and none implies another. The Connect page picks roles with
  independent checkboxes, edits them from Policy, and has a Prometheus section.
- **Dashboard:** the sidebar footer is three icon buttons (source on GitHub,
  theme, log out), and the interface text was rewritten in plainer language.
- **Bridge updater:** the Codex native-history gate turn is judged by recall of
  the earlier codename instead of `pong` (it refused every release since the
  check was added, including 0.167.1 and 0.167.2). An undated channel
  (Antigravity) soaks from the first sighting of any version newer than current,
  so daily releases no longer starve updates (1.4.12 had been held through
  1.5.0–1.5.9).

## v2.0.0 — Bridge environment cleanup, caching, Codex fidelity and security remediation

First release after v1.8.1. The subsections below are the pre-release builds that
ran in production before release; security records cite them by those numbers.

### Release review, transport and dashboard

- **Fixes:** an unset `per_client_inflight` is clamped to `global_inflight`;
  VS Code path tokens are masked from the raw request path; a Claude stream whose
  CLI fails after the visible message still ends with `message_delta`/`message_stop`;
  a refused Antigravity resume records its billed usage on the error body;
  admission reserves a declared `Content-Length` instead of `max_body`; stale
  Copilot workers are reaped after the pool lock is released; Copilot cleanup
  failures are a typed pre-inference `cached_session_cleanup`; parked Codex
  tool turns are keyed by caller, so a foreign call ID replays instead of 403.
- **Shared transport:** Codex and Copilot stream through `internal/bridgeutil`
  like Claude and Antigravity. Failed writes are sticky; Copilot no longer
  ignores them. Verified live with Codex and Copilot (`gpt-6-luna`) text and
  tool streams.
- **Ledger:** compression stage attribution is stored only when a stage changed
  something (an all-zero list was a third of the table), and
  "upstream attempts" notes only when a request retried. Schema v35 drops
  `calls.tool_prune_shadow_tokens` from the shelved tool-pruning experiment;
  its 2,875 measurements (11,850,055 shadow tokens, Oct 31 to Nov 4) were
  exported first. The migration runs
  once on startup and rewrites the `calls` table.
- **Request explorer:** `/admin/requests` adds `summary.empty_steps` and a
  `hide_empty` filter; the dashboard shows a "no tokens" badge for zero-token
  steps (code-mode tool steps, or usage reported later in the turn) and a
  toggle to hide them.
- **Dashboard:** bridge cards list the exact `~/.<bridge>-sub` sign-in and check
  commands; the favicon and sidebar mark use the shared ez icon.
- **Measured, no change needed:** Codex usage in code mode arrives after the
  script's tool calls finish, so it lands on a later row (totals correct); Codex
  already batches independent tool calls.

### Copilot bridge uses a dedicated login (breaking)

- `EZLLM_COPILOT_HOME` now defaults to `/copilot-sub` instead of `/copilot-home`, so
  every subscription bridge signs in to its own `~/.<bridge>-sub` folder rather than
  the CLI's everyday home. There is no fallback: log in with
  `COPILOT_HOME=~/.copilot-sub`, mount it at `/copilot-sub` and recreate the container.
  The bridge updater's Copilot gate uses the same default.
- The README lists the sign-in command for each bridge.

### Bridge environment defaults (breaking)

- Every `*_BIN` default is now the updater-managed path,
  `/opt/<bridge>-bridge/current/...` (was `/opt/claude/claude`,
  `/opt/codex/bin/codex`, `/opt/agy/agy`, `/opt/copilot/copilot`). A compose that
  mounted a single binary at the old path must set `*_BIN` or mount the bridge root.
- `EZLLM_CLAUDE_CONFIG_DIR` is renamed `EZLLM_CLAUDE_HOME`, so every bridge's login
  folder is `EZLLM_<BRIDGE>_HOME`. The old name fails the boot with a rename message.
- `EZLLM_COPILOT_BRIDGE_TOKEN_FILE` defaults to `/data/copilot-bridge.token`, like
  the other bridges, instead of refusing to boot.
- Caching is the default on every bridge and `off` is the only opt-out.
  `EZLLM_CODEX_BRIDGE_CACHE=auto` is removed (unset caches wherever the CLI supports
  it). `EZLLM_COPILOT_BRIDGE_SESSION_CACHE` and `EZLLM_AGY_BRIDGE_SESSION_CACHE` are
  renamed `EZLLM_COPILOT_BRIDGE_CACHE` and `EZLLM_AGY_BRIDGE_CACHE`. The Copilot warm
  pool is on, and the Copilot and Antigravity caches follow their prerequisite (warm
  pool; tool adapter plus CLI `--conversation`) when unset. An explicit `on` that
  cannot work still fails the boot. The updater gate expects the same defaults.
- The Codex sandbox is always `workspace-write`; `EZLLM_CODEX_SANDBOX` and the
  read-only mode are removed.
- `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK` is removed: with a writable sandbox, Codex
  client tools always require Landlock.
- `EZLLM_CLAUDE_BRIDGE_CONCURRENCY` is new, and every bridge's concurrency now
  defaults to 1 CLI process (was 2; Claude's was fixed at 2).
- The master key defaults to `<data_dir>/ezllm-master.key` instead of
  `~/.hermes/secrets/ezllm-master.key`. A key left at the old path refuses the boot
  until it is moved; ezllm never generates a replacement over it.
- `EZLLM_USAGE_TZ` (with its dashboard/YAML setting) is the only timezone: report
  days, model rules, client-token budgets, date-only token expiries, schedules and log
  timestamps all follow it, and a dashboard change reaches the logs immediately. `TZ`
  is ignored and the image no longer sets it.
- `EZLLM_WEB_DIR` is removed: the dashboard is always the one built into the binary.
- The deprecated `EZLLM_TRANSLATION_UNIVERSAL_ENABLED` alias is removed;
  `EZLLM_TRANSLATION_ENABLED` is the switch.
- `compose.example.yml` no longer substitutes `EZLLM_DATA`: write the host data path
  into the `volumes` entry directly. `EZLLM_DATA_DIR` is the only data setting.
- Retired names (`EZLLM_CLAUDE_CONFIG_DIR`, `EZLLM_CODEX_SANDBOX`,
  `EZLLM_CODEX_TOOLS_WITHOUT_LANDLOCK`, `EZLLM_WEB_DIR`,
  `EZLLM_TRANSLATION_UNIVERSAL_ENABLED`, both
  `*_SESSION_CACHE`) fail the boot with a message naming the replacement.

### Codex conversation fidelity (pre-release 1.14.3)

- Uses native history injection when advertised by the CLI, keeping user and
  assistant roles, function-call/result links, assistant phases and image order
  instead of embedding the entire conversation in one user transcript.
- Preserves explicit assistant phases; reconstructs commentary for tool-bearing
  Chat Completions messages and final-answer phase for completed responses.
- Injects caller history only after the request fork, leaving shared templates
  empty. Pending caller tools retain their existing authenticated continuation.
- Updates CLI qualification to prove native history and reject older qualification
  policies. Adds protocol regressions and a bounded, explicitly gated live task comparison.
  Small synthetic tasks complete with cache both on and off; the original long
  Hermes-session stopping behavior is not reproduced by those tests.

### Codex task persistence guidance (pre-release 1.14.2)

- Tells tool-bearing Codex requests to continue authorized implementation and
  verification after environment checks, inspection and planning. A final report
  that work has not started is not a substitute for implementation; concrete
  blockers and user pauses still stop work.
- Verifies instructions reach native threads with empty-template caching enabled
  and disabled, while plain chat retains its original instructions. There is no
  new env flag, forced tool call or automatic retry loop.
- Measured afterwards (2026-11-14 Hermes replays): this guidance did not change
  `gpt-6.1-terra` ending long tasks early; switching the model to `gpt-6.1-sol`
  did. See `docs/codex-bridge-runbook.md` "Model choice for long caller-tool tasks".

### Security audit remediation (pre-release 1.14.1)

- Bounds inference admission before body allocation: default 32 global/eight
  per-client active requests and 256 MiB of input reservations. Excess work
  receives retryable 429; reservations are released on completion, cancellation
  and panic. Limits are configurable and cannot be disabled with zero.
- Strips provider cookies and origin-state headers on public and dashboard
  inference responses. Auth and panic logs redact path credentials, including
  encoded slash tokens; panic payloads are not logged.
- Binds pending Codex tool conversations to gateway-authenticated callers and
  full canonical history. Spoofed identity headers never establish ownership,
  and recycled native IDs cannot replace another parked conversation.
- Shares the bounded/deadlined admin decoder for settings writes.
- Pins Go 1.27.4 and adds a reachable-vulnerability CI check. Compose examples
  bind HTTP to localhost; private deployment files are excluded from image builds.
- Replaces the audit's vulnerable-behavior probes with permanent rejection,
  redaction, cleanup and continuation regressions. Runtime rollout is separate
  from the source fixes.

### Automatic Codex caching and noninteractive CLI defaults (pre-release 1.14.0)

- Defaults `EZLLM_CODEX_BRIDGE_CACHE` to `auto`: enable supported empty-template
  forks at startup, otherwise expose the reason and use the original path.
  `on` requires support; `off` opts out. Invalid values are configuration errors.
- Reports the requested mode, effective decision and fallback reason on `/status`.
  Updater policy proofs now cover auto/on/off and `approval_policy=never`.
- Pins approval policy to `never` in CLI configuration for all app-server and exec
  children, in addition to existing thread creation/resume settings. Prediction
  forks inherit that policy. Caller-tool instructions continue authorized work
  without repeated permission questions; actual caller approval requirements apply.

### Codex prompt-cache affinity across fresh processes (pre-release 1.13.0)

- Adds `EZLLM_CODEX_BRIDGE_CACHE=on`: persisted empty configuration templates
  feed separate ephemeral prediction forks. Every independent request still
  supplies its full caller transcript; no caller dialogue enters the parent.
- Reuses private scratch paths while scrubbing contents after process exit.
  Keeps no idle CLI process, new service or dependency. Limits idle entries to
  concurrency, with one-minute idle and 15-minute/100-request lifetime bounds.
- Preserves native pending-tool callbacks and usage deltas. Changed configuration
  cannot resume a cached tool turn. Parent notifications cannot meter a child.
- Deletes retired parents via native control calls, bounds cleanup credits and
  reports counts on loopback `/status`. Updater qualification proves cache mode.
- Restricts automatic exec retry to known pre-inference failures; silent failure
  after `turn/start` cannot trigger a second generation.
- Native synthetic Hermes-shaped proof on CLI 0.167.0: second fresh-process
  request read 12,416 cached tokens of 12,657 input (98.1%). Both caller-tool
  cycles passed under Landlock; results are provider observations, not a quota
  reduction guarantee.

### Antigravity conversation cache and usage correction (pre-release 1.12.0)

- Adds `EZLLM_AGY_BRIDGE_SESSION_CACHE=on`, requiring the experimental caller-tool
  adapter. Exact histories resume the native conversation in its private workspace;
  only the new user turn or complete caller tool-result group is sent. Tool-call
  IDs remain stable and the caller still executes all tools.
- Retains at most one idle entry per concurrency slot, with 1 MiB history limits,
  one-minute idle expiry and 15-minute/100-turn age limits. No idle CLI process,
  new service, dependency or independent on-disk cache is added.
- Corrects AGY usage: native input excludes cache reads. OpenAI prompt totals now
  include them, while the ledger records the actual cold/read split. Resumed native
  cumulative counters are returned as per-request deltas. Historical rows are unchanged.
- Deadlines terminate native process groups even after a result, and expired
  results cannot enter the cache. Failed resumes preserve per-request usage.
- Requires resume-capability and cache-mode live proofs before CLI qualification.
  Failures, counter resets and changed native conversation IDs never trigger an
  internal repair generation.

### Bounded Copilot conversation cache (pre-release 1.11.0)

- Adds `EZLLM_COPILOT_BRIDGE_SESSION_CACHE=on`, off by default and requiring
  the existing warm pool. Exact completed-history continuations send only the
  new user turn, preserving native prompt-cache lineage without a new service,
  dependency or worker pool.
- Keeps one completed session per worker, bounded by existing idle/age/turn
  limits and 1 MiB of normalized history. Moved Hermes cache markers are
  treated as transport metadata; changed text, model, system or tools cannot hit.
- Bills deltas of native cumulative counters. Pending tool batches are accounted
  and deleted; images, automatic model selection and tool-result continuations
  use the fresh-session path. Idle eviction and graceful close delete sessions.
- CLI updater gates verify completed-session continuation when the mode is on.
  Real Gemini Flash Antigravity probes showed no cache reads; Codex stable-cwd
  and Copilot recreated-ID probes did not improve reuse. The retained Copilot
  HTTP sample reported roughly 90% lower credits on its continuation, which
  is a small-model sample rather than a production-wide savings guarantee.

### Bridge usage accounting and fallback guards (pre-release 1.10.1)

- Copilot's per-model input fallback no longer counts cached input twice,
  includes all reported models, and preserves reasoning as a subset of output.
- Plain Copilot requests only fall back to `-p` after a proven pre-inference
  refusal with no observed work; send-phase failures cannot authorize a retry.
- Claude text still streams immediately; terminal events wait for the CLI result
  and report aggregate usage, including any native internal retry calls, while
  keeping the first message's content and stop reason.
- Live Hermes-shaped probes additionally preserve Copilot cache-write counters
  on the wire and Claude terminal thinking details; recorded counters exercise
  the ledger usage tap without additional vendor calls.
- Records the Hermes-only historical findings and remaining measurement limits
  in `docs/design/bridge-token-usage-audit.md`. No new runtime dependency is added.

### Bridge updater reliability and coverage (pre-release 1.10.0)

- Copilot can join the existing versioned-root updater: stable official GitHub
  assets, SHA-256 verification, staging, live gate, grouped restart and proof.
  It remains disabled in updater defaults until its mount is migrated.
- Promotions use a durable recovery journal and roll back the entire batch
  when any proof fails or raises. An interrupted transaction is recovered on
  the next run; failed recovery retains the journal and exits nonzero.
- Gates use the running image ID, actual login mount, configured user/network,
  and a private temporary environment file. Policy verdicts must match Codex
  sandbox mode, Antigravity caller-tool mode and Copilot warm-pool mode.
- Quota, auth and transport refusals wait for a configurable retry instead of
  permanently blocking a CLI release. Known legacy quota blocks migrate to
  backoff. Compatibility failures remain blocked.
- Rechecks the window before changing symlinks and keeps the newest pending
  candidate in status. Codex and Antigravity live checks prove caller result
  replay; fixes a Copilot live-check panic on an ordinary answer without tools.

### Experimental Antigravity caller tools (pre-release 1.9.0)

- `EZLLM_AGY_EXPERIMENTAL_TOOLS=on` enables a structured-output adapter for
  function tools. Tool calls return to the calling harness for execution;
  assistant calls and matching tool results replay into a fresh CLI process
  on the next request. No ACP, MCP, sidecar, new dependency or parked process.
- Supports auto, none, required and named function choice, plus
  `parallel_tool_calls=false`. Invalid envelopes return `502 invalid_tool_output`
  before any streaming output. Output is bounded to 1 MiB and 16 calls per turn.
- Plain chat retains its existing behavior. Images and legacy functions remain
  unsupported. The CLI must expose `--json-schema`; the feature is off by default.

### Codex bridge caller tools and headless permissions (pre-release 1.8.2)

- Tool-bearing requests distinguish the bridge's local sandbox from the
  caller's tool permissions. Authorized workspace writes use caller tools;
  home-directory resolution uses the caller's environment. Permission errors
  come from actual tool results, rather than assumptions about the bridge.
- Writable mode uses noninteractive approval policy `never`; the exec fallback
  passes approval policy through a supported config override. Native tools
  remain disabled and Landlock confinement stays in force.

## v1.8.1 — Copilot wire probe fix

- Copilot key, wire and combo probes omit output-token caps that the CLI cannot
  enforce. Parameter refusals no longer count as evidence of an unsupported
  wire. Re-run the wire probe after upgrading to replace an earlier failed
  verdict with a measured result before pinning the chat wire.

## v1.8.0 — Meta Model API, dependency updates and bridge improvements

### Copilot image input

- Inline PNG/JPEG `image_url` parts map to native in-memory CLI blobs. Numbered
  markers preserve image ordering in the flattened transcript; base64 is not
  included in prompt text. Remote URLs and file paths are refused.
- Explicit model vision capability and MIME/count/size checks run before
  inference, alongside bridge byte/pixel bounds. `auto` and non-auto detail
  settings are refused; image requests cannot fall back to text-only `-p`.
- Multimodal user metadata no longer gets decoded as assistant text. Consumed
  event validation and session isolation remain enforced. Live HTTP checks on
  `gpt-6-luna` passed in both response modes with positive usage.

### Shared bridge documentation

- Updated the overview, code tour and four bridge guides with migration status.
  Claude and Antigravity use the shared stream transport; Codex and Copilot
  expansion remains outstanding. The shared design records per-bridge scope
  and acceptance checks for those follow-up migrations.

### Shared transport for Claude and Antigravity

- Extracted protocol-independent streaming bytes, flush, cancellation and write
  deadlines into `internal/bridgeutil`. Claude retains Anthropic events;
  Antigravity retains OpenAI chunks, header timing and usage mapping.
- Claude now bounds stalled client writes. Both adapters stop emitting after
  transport failure and join cancellation callbacks before clearing deadlines.
- Antigravity passed live CLI and HTTP checks before migration and live HTTP
  streaming/non-streaming checks afterward on the current CLI 1.4.12.

### Opt-in Copilot warm processes

- `EZLLM_COPILOT_BRIDGE_WARM_POOL=on` reuses CLI processes under the existing
  concurrency and queue limits. Every request still creates a fresh session
  with its own system message and explicit client-tool allowlist.
- Only confirmed idle/usage/delete cleanup permits reuse. Cancellation, uncertain
  cleanup, protocol errors, worker death and binary replacement retire workers;
  possibly billed inference is never automatically replayed. Session IDs filter
  delayed events and cleanup timers are joined before a worker can be leased.
- Workers expire after one idle minute, 15 minutes of age or 100 successful turns.
  Bridge shutdown drains HTTP work then terminates and reaps all warm children.
  Status reports pool state alongside stage timings. Pooling defaults off.

### Copilot tool streaming and latency diagnostics

- Complete declared tool batches stream before abort/usage collection and session
  deletion. Usage, finish reason and stream completion follow cleanup; batches
  are emitted once and non-stream requests retain full-response semantics.
- Copilot bridge status includes aggregate queue, spawn, RPC, model-wait,
  first-event, tool-batch and usage-drain timings. No request content is retained.
- Controlled live text and tool calls succeeded with usage. Measured startup
  and session creation dominated cleanup in those small requests; see the
  Copilot latency diagnostic note for measurements and limits.

### Meta Model API and dependency refresh

- Added Meta Model API at `https://api.meta.ai/v1` using existing model discovery
  and protocol probes. Updated Go and dashboard dependencies.
- Reject successful provider JSON responses over 8 MiB instead of accepting a
  truncated valid JSON prefix. HTTP failures retain their status classification.
- Removed an orphaned dashboard comment from the retired tool-pruning feature.

## v1.7.0 — renamed: provider presets say what they are

- **The four subscription bridges are labelled as bridges** in the Add-provider
  dropdown: `Claude Pro/Max (bridge)`, `ChatGPT Plus/Pro (bridge)`,
  `Google Antigravity (bridge)`, `GitHub Copilot (bridge)` (was "GitHub Copilot
  (CLI)"). Those four need a signed-in CLI and take no key — that is the
  difference that changes what happens after Save, so it belongs in the name.
- **The two web-session presets drop the mechanism from their name:**
  `Claude.ai (web session)` → **`Claude.ai (web)`**, `Google Gemini (web
  cookies)` → **`Google Gemini (web)`**. The cookie field below the dropdown
  already says how.
- Account names are unaffected: the `(bridge)` suffix is stripped when a name is
  auto-seeded, so a card still reads "GitHub Copilot", not "GitHub Copilot
  (bridge)". Existing accounts keep the names they already have.

## v1.7.0 — fixed: Codex tool calls aimed at the bridge's scratch directory

- **Symptom:** a tool-using client (a VPS status sweep) got "this environment can't
  reach its shell" from the Codex hop while the Claude hop ran the same tools.
  Codex's own log showed the model DID call the client's `terminal` tool, twice,
  and both calls failed with exit 126: `cd: /tmp/codex-bridge-…/cwd: No such file
  or directory`.
- **Cause:** the Codex child runs in a private scratch directory and says so in
  its context; the model passed that directory as the shell tool's `workdir`, a
  path that exists only inside the bridge. After two failures it concluded it had
  no shell.
- **Fix:** requests that carry client tools get a short instruction (`ClientToolsNote`)
  saying the tools run on the caller's machine and not to pass a working
  directory the user did not give. Measured live against the real CLI with the
  same tool-using request: the scratch directory leaked into the tool call on
  3 of 3 runs before, 0 of 5 after. Plain chat is unchanged.
- **The client had a stale shell directory too, and `cd ~` cannot fix it.** The
  first failing path in the trace, `…/frontend`, was the
  client's own saved working directory, gone from disk. Its terminal wrapper
  `cd`s there BEFORE running the command, so a model that prefixed `cd ~ &&`
  (the first version of the note told it to) failed identically and gave up
  ("the status command is failing before it runs"). The Claude hop's model
  recovered by passing an explicit working directory. The note now says that: on
  `cd: …: No such file or directory`, call the tool again with its `workdir` set
  to a directory that always exists, such as `/`. Measured on the real CLI on the
  transcript of two such failures: the previous note ran `pwd` and failed again on
  4 of 4 runs; the new one passed `workdir="/"` on 4 of 4. The stale directory
  itself is a client defect (workaround: recreate that directory).

## v1.7.0 — replaced: GitHub Copilot OAuth → the official CLI bridge

- **The OAuth provider is gone, not deprecated.** `internal/githubcopilot`
  (device flow, session minting, token refresh), `internal/provider/githubcopilot.go`
  with the `github-copilot` kind, `cmd/ezllm/githublogin.go`, the
  `/admin/github/device*` routes with `Server.ghDevices`, `keyTestRequest.DeviceHandle`,
  the `KnownWireSupport` hook it alone used, and the dashboard's login panel +
  "GitHub Copilot" preset are all deleted. `config.yaml` no longer accepts the kind.
- **In its place:** the `copilot-bridge` kind over the GitHub Copilot CLI
  (1.0.105), a fourth member of the claude/codex/agy bridge family. No key to
  paste: the credential is the CLI's own login. Design
  `docs/design/copilot-cli-bridge-design.md`, runbook `docs/copilot-bridge-runbook.md`.
- **Accounting is exact for the first time on this provider:** the CLI's
  `session.usage_checkpoint` gives inclusive prompt tokens with a cached slice;
  verified end-to-end through the gateway (bridge 13,971 inclusive / 9,420
  cached → ledger `tokens_in 4,551` + `tokens_cached_read 9,420` + `tokens_out 6`).
- **Every turn runs on the CLI's headless channel, without its agent scaffolding.**
  `copilot -p` costs ~13–14k prompt tokens even for a one-word answer (≈9.4k is the
  CLI's own tool schemas). The channel replaces the system message and shows the
  model only the caller's tools: the same kind of short question measured 95 prompt
  tokens against 13,957. If the channel is unavailable, plain chat falls back to
  `-p`, so the bridge still serves it. Only that refusal falls back; a failure at
  the CLI is not repeated, because that would charge for it twice.
- **Client tools are native.** `-p` has no option that yields a pending tool
  call; `copilot --headless --stdio`, the JSON-RPC channel the CLI's bundled SDK
  uses, does. The model calls the caller's tools
  natively, the bridge returns the batch as OpenAI `tool_calls` with the CLI's own
  call ids and stops the turn, and the results come back on the next request as
  a fresh session with the history replayed as text. Only the caller's tools are
  visible, so nothing the caller did not declare can run on the box; a caller's
  tool named `bash` goes back to the caller. Text streams live. Each turn checks
  the channel's protocol version first, and a mismatch, a missing `--headless` or
  an unregistrable tool name is, on a request with tools, a 400
  `chat_tools_unsupported` before anything is billed, which the router fails over
  on with no cooldown (`refusesTools`).
  `tool_choice`: `none` is a plain turn with no tools visible, and `required` or
  a named function is refused up front (400, before billing) so another hop can
  enforce it — the channel has no equivalent to force a call. This replaces two
  earlier cuts from the same release:
  a refusal, which returned 400 on a live combo's first request, and a `<tool_call>`
  text protocol, which the model ignored and which charged for empty turns.
- **`ezllm bridge-check copilot` exists** (static: binary, version, and the
  headless channel's protocol version, which is the one thing a release can move
  without a version number; `--live`: the login, a plain turn, and a client-tool
  batch plus its replay). The static tier deliberately does NOT claim a login:
  an empty `COPILOT_HOME` still passes `session.create`, so promotion requires
  `--live`. A batch of undeclared tool names now fails with the names it
  dropped instead of "finished without an assistant message".
- **Usage no longer depends on how the CLI writes a number.** The channel writes
  `totalPremiumRequestCost` as `1.0`, which an int field refused, dropping the
  whole event and booking zero usage. A fractional multiplier would have done the
  same on `-p`. `copilot_nano_aiu` now rides `raw_usage` for AI-credit plans.
  Design §10 addendum.
- **Pressure is bounded and visible.** `EZLLM_COPILOT_BRIDGE_CONCURRENCY`
  (2), `_QUEUE` (8) and `_TIMEOUT` (300s) are configurable; past the queue the
  bridge answers 429 `queue_full` + `Retry-After: 5` instead of parking the
  caller. `/status` gains a `runtime` block: in-flight, queued, rejected,
  per-outcome counts and CLI first-byte latency. The CLI child runs on an
  allowlisted environment, never ezllm's own secrets.
- **The model catalog grows itself, for free.** The CLI reports the ids a login
  can use (`availableModels`) on every `--model auto` turn; the bridge folds
  valid ids into `/v1/models` after the curated seed (so the key test still
  probes a proven id first) and shows them as `learned_models` on `/status`.
  Nothing is probed, so no premium request is spent on discovery. In-memory:
  a restart relearns on the next auto turn.
- **The catalog is every id the CLI documents.** At boot the bridge reads
  `copilot help config` (local, free) and lists all of them after `auto`, with a
  built-in copy as fallback; the plan decides, and a model it lacks answers
  403 `model_not_in_plan` (the pre-spawn refusal of an unknown id moved from
  400 `unknown_model` to the same 403).

## v1.7.0 — removed: tool pruning (rejected feature)

- **Deleted the whole feature**, per the standing rule that a feature no live
  data uses is removed outright rather than deprecated: `internal/toolprune`
  (policy + tracker + tests), the `inference()` shadow hook,
  `internal/server/toolusage.go` and `GET /admin/tool-usage`, the store stats
  layer, the `tool_pruning` config block with its env override and validators,
  the dashboard totals row and the feed/summary field, and
  `docs/tool-pruning-design.md`.
- **Kept, frozen and documented**: `calls.tool_prune_shadow_tokens` and the
  `tool_usage` table stay in the schema. Ledger rows are immutable history and
  the rows written during the four-day measurement are the evidence the
  rejection rests on; dropping them is a destructive migration nobody asked
  for. Nothing reads or writes either any more.
- Why it went: **M2 was rejected 2026-11-04** on the four-day M1 evidence, and
  the design doc went with the feature, so the verdict lives here.
  - **The prize was history.** Of 10.5M cumulative `prize_tokens`, 9,446,068
    (89.8%) came from tools that stopped shipping on 2026-11-01 (Hermes allowlist
    41→8 + `tool_search.defer`). A `used_sessions == 0` row for a tool that no
    longer ships can never flip to used, so it looks eligible forever — any
    future go/no-go must filter on `last_seen_at`, not `eligible`.
  - **The live tax was noise.** 481 tokens per request (mnemosyne_invalidate 117,
    update 113, get 110, skills_list 77, forget 64), i.e. one request in six
    hundred; the 24h gate reading decayed 3.9% → 2.7% → 1.7% → 0.9% on the only
    days that reflect the trimmed surface, below the 2–3% trigger.
  - **Silent capability loss is what killed it, and M1 could not even see the
    usage that would have prevented it.** The top candidate by prize,
    one client's report tool (1,948 tok/req × 446 requests) is that
    client's flagship tool, yet it read `used_sessions = 0`: usage is derived
    from call sites in the *request* body, and that client's only model path
    sends one user message and reads the `tool_use` block out of the *response*.
    All 23 of its tools sat at zero while it used tools on essentially every
    request — prune mode would have deleted the entire catalog of the one client
    that uses tools every time, from evidence structurally blind to it. The 20%
    audit window only recovers in an audit *session*, and the router by its own
    design admission cannot observe why a run broke.
  - **The economics never reached the hot path:** ~60k tokens/day of live shadow,
    about half cache-read, single-digit dollars across the series — against
    schema v34, a `tools` rewrite between compression and `captureShapeDoc`, a
    session memo and an audit UI.
  - **All four clients are ours.** Pruning exists to trim catalogs an operator
    does not own; for catalogs we own a config line is strictly better —
    deterministic, reversible, no cache surprise, no new failure mode.
  - **Replaced by:** (1) source-side trimming in each client's own tool config
    (`tool_search.defer`, allowlists) where the use-rate math justifies it, and
    (2) optionally a per-client static deny list in ezllm if a client drifts back
    to shipping a heavyweight unused catalog. **Re-open only if** a client we do
    not control ships a large never-used catalog, or live dead tax exceeds ~2%
    of that client's prompt — and then scope per client (canary first) and probe
    all three wire shapes with a `tool_use` naming an undeclared tool first.
- See also: EXPLORING §7 lists tool pruning among the deliberate non-goals.

## v1.7.0 — hop stream health (schema v34)

- `calls.stream_outcome` records how a STREAM ended — `completed`, `truncated`,
  `failed`, `client_gone`, empty for a non-stream request — so a hop that
  returns `200` and then dies mid-body is a fact in the ledger instead of prose
  in `err` (`docs/design/hop-health-design.md`). Precedence: `client_gone` first (a
  caller hanging up is never a hop fault; that artefact once made 442 complete
  calls read as failures), then a translated stream's frame-level `failed`.
- **Fixed the reason this mattered:** the proxy set `StreamOutcome` only for
  translated streams, so a native stream that died left it empty,
  `StreamCompleted()` answered true and hop health treated a cut answer as
  delivered — no strike, no park. Measured live: 16 mid-stream resets on
  `command-code · xiaomi/mimo-v2.6-pro` in 8 days (2.1% of that hop's traffic,
  against 0/754 for the same model on `opencode-go`), none of which had ever
  counted. The `copyErr` branch now records `StreamOutcomeTruncated`.
- `/admin/health` gains `hop_stream_health` (`?stream_window=`: default 24h,
  capped 30d, bounded `truncation_rate = truncated / (streams − client_gone)`,
  worst hop first); `/admin/overview` carries the same key so the dashboard can
  read it.
- Dashboard: a **Cut answers** card with the rate chip and "N cut of M", and a
  needs-attention item above 1% over at least 20 judged streams. Row-level
  outcomes ride the feed payload (`stream_outcome`).
- No routing action yet: the park rule is the next commit and is justified by
  this measurement, not by a plausible number (the M2 lesson).
- **The bell rings for it.** Every 15 minutes the ledger rollup is checked with
  the dashboard's own thresholds (over 1% truncated, at least 20 judged streams,
  24h); an offending hop raises one `stream_truncation` warning, folded per
  (account, model) so a recurring cut is one line with a counter, and pushed
  through the existing channels. Toggle: Settings → Notifications → "Answers cut
  mid-stream" (`notify_stream_truncation`, default on).

## v1.7.0 — tool pruning M1: measurement (TokenEconomizer-inspired)

- `tool_pruning.mode: off|shadow` (default `off`; `EZLLM_TOOL_PRUNING_MODE` is
  the no-rebuild override) measures what never-used tool schemas cost per
  request — the first, zero-risk half of the TokenEconomizer port
  (`docs/tool-pruning-design.md`). **Shadow mode never rewrites a body.**
- `tool_usage` stats (schema v33), one row per `(client, tool)`: schemas
  shipped vs tools the traces actually call, session-pair counted. The policy
  (`used_sessions == 0 && shipped_sessions >= min_unused_sessions && !keep`,
  used verdicts sticky lifetime) books its potential to the new ledger column
  `calls.tool_prune_shadow_tokens`, threaded through the requests summary,
  the feed projection and the dashboard totals.
- `GET /admin/tool-usage` — the per-tool prize rollup
  (`summary.prize_tokens` = what pruning would remove from every future
  request) that gates the M2 go/no-go; `prune` mode itself is M2 and does not
  exist yet.
- Eligibility hardening (review fixes, before any production shadow run):
  `Tracker.Deltas` holds its lock for the whole call (two concurrent requests
  of one session raced on its maps — a fatal concurrent map write);
  `server_tool_use` / `mcp_tool_use` mark their tool used (Anthropic server
  tools were otherwise never "used"); a tool result whose call site is not in
  the body (`previous_response_id`, a compacted history) empties that
  request's eligible set and withdraws its session's shipped-but-unused pairs
  (`shipped_sessions` −1); a tool called in the request's own trace is never
  eligible, whatever the pre-request stats say.
- Tracking persistence fixes: catalog-free turns still record visible calls
  and unresolved-result withdrawals. `Tracker.Apply` commits session counters
  only with a successful stats transaction; failed writes book zero shadow
  tokens and leave pair transitions retryable on later observations. Regression
  tests cover known usage, unresolved results, canceled writes and deduplicated
  retries. No background replay is added.
- Verifier hardening: the UsageRow↔api.ts contract test's path went stale when
  the file moved to `web/src/utils/api.ts` and had been silently SKIPPING
  since — fixed, the pin bites again.

## 2026-10-31 — bridge CLI auto-update and push channels (v1.6.0)

- `ezllm bridge-check {claude|codex|agy} [--bin] [--home] [--json]` runs a
  bridge's boot checks against a staged CLI and refuses on any problem, the
  first milestone of unattended CLI updates (`docs/design/SPEC-bridge-update.md`).
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
- Push channels (schema 32): bell events to ntfy, Discord, Telegram or a JSON
  webhook from Settings → Notifications, with minimum severity, Send test
  (drafts too) and delivery status; secrets encrypted, write-only and covered
  by backup verification; one background worker, never on the request path.
  `/admin/notification-channels` CRUD + `…/test`. Browser driver
  `verify-push-channels.mjs`.
- Neighbourhood: a loading bar while the providers load and while the city's
  first frame is drawn, instead of a blank canvas; it fades once the scene is
  on screen and never takes a click.
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
`demo-main` and `initech-gpt` as *enabled accounts with no key*, which is also
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
one combo/anthropic), and eight later requests were answered "all combo endpoints
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
  `docs/design/antigravity-subscription-design.md`; setup: `docs/agy-bridge-runbook.md`.
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
  (`docs/codex-bridge-runbook.md`, `docs/design/codex-subscription-design.md`).
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
  commented) to `compose.yml`, which is gitignored. the reverse-proxy overlay is gone;
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
  (`docs/design/claude-subscription-design.md` §2.1, §6).
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
  (`docs/design/services-design.md`).
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
(`docs/design/claude-subscription-design.md`, locked decision 1).

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
`docs/design/claude-web-design.md`, runbook: `docs/claude-web-runbook.md`).
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
implementation (its design, policy-evidence and runbook docs were
removed after the provider was retired; see git history). The auth path is the OFFICIAL CLI's:
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
from the reverse-engineered reference (design: `docs/design/gemini-web-design.md`, runbook: `docs/gemini-web-runbook.md`).
Unofficial surface; risk posture accepted by the owner: cookie material = live
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
  `docs/design/single-parse-dispatch-design.md`.
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
- Distroless image + reverse-proxy overlay; explicit router priorities and a dedicated
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
